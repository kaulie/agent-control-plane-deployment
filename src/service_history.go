package main

import (
	"database/sql"
	"fmt"
	"strings"
)

// ServiceHistoryCounts 是一个 serviceId 名下历史记录的条数。迁移/清除旧契约前
// 用它给调用方（面板）一个明确的确认依据，也用它拦住在途任务。
//
// 背景：服务目录以 service_registry 为唯一真源，本机 SQLite 只是「部署配置」。
// 但流水线 / 部署 / 制品索引都是按 serviceId 存的 —— 早期（注册中心接入前）本机
// 自建的契约（如 web-cursor）和注册中心里的服务（如 agent-control-plane）是同一个
// 服务的两个 id。把旧契约的历史迁到新 id 上，才能既删掉旧契约、又不丢历史。
type ServiceHistoryCounts struct {
	Pipelines int `json:"pipelines"`
	Deploys   int `json:"deploys"`
	Artifacts int `json:"artifacts"`
	// InflightPipelines: queued / packaging / deploying 的流水线。
	InflightPipelines int `json:"inflightPipelines"`
	// InflightDeploys: queued / running 的部署任务。
	InflightDeploys int `json:"inflightDeploys"`
}

// Inflight 是在途任务总数：>0 时不允许迁移历史，也不允许删除契约。
func (c ServiceHistoryCounts) Inflight() int {
	return c.InflightPipelines + c.InflightDeploys
}

// HasHistory 报告该 serviceId 名下是否还有历史记录（流水线 / 部署 / 制品索引）。
func (c ServiceHistoryCounts) HasHistory() bool {
	return c.Pipelines > 0 || c.Deploys > 0 || c.Artifacts > 0
}

// ErrServiceInflight 表示该服务还有在途任务。两种操作都必须先等任务结束：
//   - 迁移历史：会把「正在跑的任务」也搬到目标服务名下，而部署 worker 是按
//     job.service_id 查契约拿 runtimeDir / 启停命令的 —— 搬走就等于换了个服务跑；
//   - 删除契约：排队中的任务被 worker 认领时会找不到契约，直接失败。
type ErrServiceInflight struct {
	ServiceID string
	Counts    ServiceHistoryCounts
}

func (e *ErrServiceInflight) Error() string {
	return fmt.Sprintf(
		"服务 %s 还有 %d 个在途任务（%d 个流水线 / %d 个部署）；等它们结束后再迁移或清除",
		e.ServiceID, e.Counts.Inflight(), e.Counts.InflightPipelines, e.Counts.InflightDeploys)
}

// CountServiceHistory 统计一个 serviceId 名下的历史记录与在途任务。
func (s *Store) CountServiceHistory(serviceID string) (ServiceHistoryCounts, error) {
	var c ServiceHistoryCounts
	err := s.db.QueryRow(`
		SELECT
		  (SELECT COUNT(*) FROM pipelines WHERE service_id = ?),
		  (SELECT COUNT(*) FROM deploys   WHERE service_id = ?),
		  (SELECT COUNT(*) FROM artifacts WHERE service_id = ?),
		  (SELECT COUNT(*) FROM pipelines WHERE service_id = ? AND state IN ('queued','packaging','deploying')),
		  (SELECT COUNT(*) FROM deploys   WHERE service_id = ? AND state IN ('queued','running'))`,
		serviceID, serviceID, serviceID, serviceID, serviceID,
	).Scan(&c.Pipelines, &c.Deploys, &c.Artifacts, &c.InflightPipelines, &c.InflightDeploys)
	if err != nil {
		return c, err
	}
	return c, nil
}

// ServiceHistoryMove 是「把一个 serviceId 的历史记录挪到另一个 serviceId」的结果。
type ServiceHistoryMove struct {
	From             string `json:"from"`
	To               string `json:"to"`
	Pipelines        int    `json:"pipelines"`
	Deploys          int    `json:"deploys"`
	Artifacts        int    `json:"artifacts"`
	ArtifactsSkipped int    `json:"artifactsSkipped"`
	// DeletedContract: 迁移后是否顺手删掉了源 serviceId 的本机部署配置。
	DeletedContract bool `json:"deletedContract"`
}

// MoveServiceHistory 把 from 名下的流水线 / 部署 / 制品索引改挂到 to 名下，并在
// deleteSourceContract=true 时删除 from 的本机部署配置。整个过程在一个事务里，
// 要么全成、要么全不动。
//
// 有在途任务时直接返回 *ErrServiceInflight（见上面的说明），不做任何写入。
// 源 serviceId 不需要还存在本地契约（历史记录可以是一个已删契约的孤儿）。
func (s *Store) MoveServiceHistory(from, to string, deleteSourceContract bool) (ServiceHistoryMove, error) {
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	move := ServiceHistoryMove{From: from, To: to}
	if from == "" || to == "" {
		return move, fmt.Errorf("from and to are required")
	}
	if from == to {
		return move, fmt.Errorf("from and to are the same service: %s", from)
	}

	counts, err := s.CountServiceHistory(from)
	if err != nil {
		return move, err
	}
	if counts.Inflight() > 0 {
		return move, &ErrServiceInflight{ServiceID: from, Counts: counts}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return move, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(`UPDATE pipelines SET service_id = ? WHERE service_id = ?`, to, from)
	if err != nil {
		return move, err
	}
	if move.Pipelines, err = rowsAffected(res); err != nil {
		return move, err
	}

	res, err = tx.Exec(`UPDATE deploys SET service_id = ? WHERE service_id = ?`, to, from)
	if err != nil {
		return move, err
	}
	if move.Deploys, err = rowsAffected(res); err != nil {
		return move, err
	}

	moved, skipped, err := moveArtifactIndex(tx, from, to)
	if err != nil {
		return move, err
	}
	move.Artifacts, move.ArtifactsSkipped = moved, skipped

	if deleteSourceContract {
		if _, err := tx.Exec(`DELETE FROM services WHERE service_id = ?`, from); err != nil {
			return move, err
		}
		move.DeletedContract = true
	}

	if err := tx.Commit(); err != nil {
		return move, err
	}
	return move, nil
}

// moveArtifactIndex 把制品索引行挂到目标服务上。artifacts 有 UNIQUE(service_id, tag)：
// 目标已经有同一个 tag 的索引行时跳过（那两行指的是同一个制品，下载走的是行里存的
// accessPath）—— 跳过行保留、不做破坏性删除，只把它计入 skipped。
func moveArtifactIndex(tx *sql.Tx, from, to string) (moved, skipped int, err error) {
	rows, err := tx.Query(`SELECT id, tag FROM artifacts WHERE service_id = ?`, from)
	if err != nil {
		return 0, 0, err
	}
	type artifactRow struct {
		id  int64
		tag string
	}
	var src []artifactRow
	for rows.Next() {
		var a artifactRow
		if err := rows.Scan(&a.id, &a.tag); err != nil {
			_ = rows.Close()
			return 0, 0, err
		}
		src = append(src, a)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, 0, err
	}

	for _, a := range src {
		var one int
		err := tx.QueryRow(`SELECT 1 FROM artifacts WHERE service_id = ? AND tag = ?`, to, a.tag).Scan(&one)
		switch {
		case err == nil:
			skipped++
			continue
		case err != sql.ErrNoRows:
			return moved, skipped, err
		}
		if _, err := tx.Exec(`UPDATE artifacts SET service_id = ? WHERE id = ?`, to, a.id); err != nil {
			return moved, skipped, err
		}
		moved++
	}
	return moved, skipped, nil
}

func rowsAffected(res sql.Result) (int, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}
