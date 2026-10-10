package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type DeployState string

const (
	StateQueued    DeployState = "queued"
	StateRunning   DeployState = "running"
	StateSucceeded DeployState = "succeeded"
	StateFailed    DeployState = "failed"
	StateCancelled DeployState = "cancelled"
)

type ServiceContract struct {
	ServiceID  string `json:"serviceId"`
	Name       string `json:"name"`
	RuntimeDir string `json:"runtimeDir"`
	// RuntimeDirs 按平台覆盖 runtimeDir（键归一成 darwin / linux）。部署时按**目标机器**
	// 的平台选（本机 = 控制面 GOOS，远端 = 预检 probe 的 uname）；没配的平台回落 RuntimeDir。
	RuntimeDirs map[string]string `json:"runtimeDirs,omitempty"`
	HealthURL   string            `json:"healthUrl"`
	// Port 是部署契约里显式声明的服务端口（本地运行端口，1..65535）。
	// 0 = 未设置 → 仍按 HealthURL 里的端口推导（老契约行为不变）。它只用于给
	// start/stop/restart 脚本传 PORT；探活仍然走 HealthURL。
	Port       int    `json:"port,omitempty"`
	StartCmd   string `json:"startCmd"`
	StopCmd    string `json:"stopCmd"`
	RestartCmd string `json:"restartCmd"`
	// Git repo used by POST /api/deploy-notify packaging.
	GitRepoURL string `json:"gitRepoUrl,omitempty"`
	// Default git branch/ref for packaging when notify omits ref (default: main).
	DefaultBranch string `json:"defaultBranch,omitempty"`
	// Project-provided graceful restart endpoints (both required to enable).
	RestartNotifyURL string `json:"restartNotifyUrl,omitempty"`
	RestartPollURL   string `json:"restartPollUrl,omitempty"`
	// 0 = use server default (GRACEFUL_RESTART_MAX_WAIT_MS).
	GracefulMaxWaitMs int `json:"gracefulRestartMaxWaitMs,omitempty"`
	// Supervise: watchdog on this machine should probe and remediate the service.
	// Stored in the existing watchdog_enabled column (historically written as 0
	// and never exposed).
	Supervise bool `json:"supervise"`
	// IntervalSec is how often watchdog probes this service. Watchdog syncs
	// this from GET /api/services; 0 in memory is stored as the factory default.
	IntervalSec int    `json:"intervalSec"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
}

const defaultProbeIntervalSec = 30
const minProbeIntervalSec = 2
const maxProbeIntervalSec = 86400

// First-wave services that should be supervised after the one-shot migration
// (watchdog_enabled was always written 0 and never shown in the API).
var defaultSuperviseServiceIDs = []string{
	"agent-control-plane",
	"service_registry",
	"agent-control-plane-deployment",
	"home-agent-brain",
	"home-agent-gateway",
}

type DeployJob struct {
	RequestID  string `json:"requestId"`
	ServiceID  string `json:"serviceId"`
	Deployment string `json:"deployment"`
	// TargetMachine 是本次部署落到的「部署机器」（目标主机/agent）。空 = 默认机器。
	// 由流水线触发时选择的机器转发而来；独立触发部署时可用请求体指定。
	TargetMachine string      `json:"targetMachine,omitempty"`
	State         DeployState `json:"state"`
	RequestedAt   string      `json:"requestedAt"`
	StartedAt     string      `json:"startedAt,omitempty"`
	FinishedAt    string      `json:"finishedAt,omitempty"`
	Version       string      `json:"version,omitempty"`
	Error         string      `json:"error,omitempty"`
	Message       string      `json:"message,omitempty"`
	// Who triggered this deploy (phase-1 identity headers). Empty = unidentified
	// (requests recorded before the feature, or IDENTITY_ENFORCE=0).
	TriggeredByRole string `json:"triggeredByRole,omitempty"`
	TriggeredByID   string `json:"triggeredById,omitempty"`
	TriggeredBy     string `json:"triggeredBy,omitempty"`
}

// Identity returns the recorded caller identity (zero value = unidentified).
func (j DeployJob) Identity() Identity {
	return Identity{Role: j.TriggeredByRole, ID: j.TriggeredByID}
}

// setIdentity fills the identity fields (and the derived "role:id" label).
func (j *DeployJob) setIdentity(id Identity) {
	j.TriggeredByRole = id.Role
	j.TriggeredByID = id.ID
	j.TriggeredBy = id.String()
}

type Store struct {
	db *sql.DB
}

func nowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

func NewStore(dbPath string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode = WAL;`); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
      CREATE TABLE IF NOT EXISTS services (
        service_id TEXT PRIMARY KEY,
        name TEXT NOT NULL,
        runtime_dir TEXT NOT NULL,
        runtime_dirs TEXT NOT NULL DEFAULT '{}',
        health_url TEXT NOT NULL,
        port INTEGER NOT NULL DEFAULT 0,
        start_cmd TEXT NOT NULL,
        stop_cmd TEXT NOT NULL,
        restart_cmd TEXT NOT NULL,
        watchdog_enabled INTEGER NOT NULL DEFAULT 1,
        interval_sec INTEGER NOT NULL DEFAULT 30,
        restart_notify_url TEXT NOT NULL DEFAULT '',
        restart_poll_url TEXT NOT NULL DEFAULT '',
        graceful_max_wait_ms INTEGER NOT NULL DEFAULT 0,
        created_at TEXT NOT NULL,
        updated_at TEXT NOT NULL
      );

      CREATE TABLE IF NOT EXISTS deploys (
        request_id TEXT PRIMARY KEY,
        service_id TEXT NOT NULL,
        deployment TEXT NOT NULL,
        state TEXT NOT NULL,
        requested_at TEXT NOT NULL,
        started_at TEXT,
        finished_at TEXT,
        version TEXT,
        error TEXT,
        message TEXT
      );

      CREATE INDEX IF NOT EXISTS idx_deploys_state ON deploys(state);
    `)
	if err != nil {
		return err
	}
	if err := s.ensureServiceExtraColumns(); err != nil {
		return err
	}
	if err := s.ensureDeployExtraColumns(); err != nil {
		return err
	}
	if err := s.migratePipelines(); err != nil {
		return err
	}
	if err := s.migratePipelineEvents(); err != nil {
		return err
	}
	if err := s.migrateArtifacts(); err != nil {
		return err
	}
	if err := s.migrateDeployEvents(); err != nil {
		return err
	}
	s.ensureServicePortUniqueIndex()
	if err := s.migrateSuperviseDefaultsOnce(); err != nil {
		return err
	}
	return nil
}

// ensureServicePortUniqueIndex 给"已指定的服务端口"加唯一约束（0 = 未指定，不参与）。
// 这是保存时唯一性校验的**兜底**：API 层会先给出友好的 409，这里防并发写入漏网。
//
// 用**尽力而为**的方式创建：老库若已经存在端口冲突的数据，建索引会失败 ——
// 那也不该让服务起不来，只记一条日志（此时仍由 API 层逐个校验）。
func (s *Store) ensureServicePortUniqueIndex() {
	_, err := s.db.Exec(
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_services_port_unique ON services(port) WHERE port > 0`)
	if err != nil {
		log.Printf("[store] warn: 未能创建服务端口唯一索引（%v）；保存时仍按 API 层校验端口唯一性", err)
	}
}

// ServiceByPort 找出"已经占用该端口"的另一个服务（excludeServiceID 用于编辑自己时排除）。
// port<=0 视为未指定，直接返回 nil。
func (s *Store) ServiceByPort(port int, excludeServiceID string) (*ServiceContract, error) {
	if port <= 0 {
		return nil, nil
	}
	row := s.db.QueryRow(`
		SELECT service_id, name, runtime_dir, runtime_dirs, health_url, port,
		       start_cmd, stop_cmd, restart_cmd, watchdog_enabled, interval_sec,
		       restart_notify_url, restart_poll_url, graceful_max_wait_ms,
		       git_repo_url, default_branch,
		       created_at, updated_at
		FROM services WHERE port = ? AND service_id <> ? ORDER BY service_id LIMIT 1`,
		port, excludeServiceID)
	svc, err := scanService(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return svc, err
}

func (s *Store) ensureServiceExtraColumns() error {
	return s.ensureColumns("services", map[string]string{
		"restart_notify_url":   `ALTER TABLE services ADD COLUMN restart_notify_url TEXT NOT NULL DEFAULT ''`,
		"restart_poll_url":     `ALTER TABLE services ADD COLUMN restart_poll_url TEXT NOT NULL DEFAULT ''`,
		"graceful_max_wait_ms": `ALTER TABLE services ADD COLUMN graceful_max_wait_ms INTEGER NOT NULL DEFAULT 0`,
		"git_repo_url":         `ALTER TABLE services ADD COLUMN git_repo_url TEXT NOT NULL DEFAULT ''`,
		"default_branch":       `ALTER TABLE services ADD COLUMN default_branch TEXT NOT NULL DEFAULT 'main'`,
		"port":                 `ALTER TABLE services ADD COLUMN port INTEGER NOT NULL DEFAULT 0`,
		// 老库补列：按平台（darwin/linux）覆盖 runtimeDir。
		"runtime_dirs": `ALTER TABLE services ADD COLUMN runtime_dirs TEXT NOT NULL DEFAULT '{}'`,
		"interval_sec": `ALTER TABLE services ADD COLUMN interval_sec INTEGER NOT NULL DEFAULT 30`,
	})
}

// ensureDeployExtraColumns adds the phase-1 identity columns to existing rows
// (unidentified deploys keep the empty default).
func (s *Store) ensureDeployExtraColumns() error {
	cols := identityColumnDDL("deploys")
	// 老库补列：本次部署的「部署机器」。
	cols["target_machine"] = `ALTER TABLE deploys ADD COLUMN target_machine TEXT NOT NULL DEFAULT ''`
	return s.ensureColumns("deploys", cols)
}

// identityColumnDDL is the shared ALTER list for the identity columns; both the
// deploys and the pipelines table carry them.
func identityColumnDDL(table string) map[string]string {
	return map[string]string{
		"triggered_by_role": `ALTER TABLE ` + table + ` ADD COLUMN triggered_by_role TEXT NOT NULL DEFAULT ''`,
		"triggered_by_id":   `ALTER TABLE ` + table + ` ADD COLUMN triggered_by_id TEXT NOT NULL DEFAULT ''`,
	}
}

// ensureColumns ALTERs in any of cols missing from table (idempotent).
func (s *Store) ensureColumns(table string, cols map[string]string) error {
	existing := map[string]bool{}
	rows, err := s.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		existing[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for col, ddl := range cols {
		if existing[col] {
			continue
		}
		if _, err := s.db.Exec(ddl); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) ensureServiceGracefulColumns() error {
	return s.ensureServiceExtraColumns()
}

func (s *Store) UpsertService(input ServiceContract) (ServiceContract, error) {
	existing, _ := s.GetService(input.ServiceID)
	ts := nowISO()
	row := input
	// 契约里的 URL 只存路径（host+port 部署时按目标机器拼）：写入前先归一。
	// 注意：这里**不**从 URL 推导端口 —— 新写入要求显式 port（API 层校验）；
	// 老契约的端口由启动迁移 NormalizeServiceURLs 补齐。
	NormalizeServiceContract(&row)
	if existing != nil {
		row.CreatedAt = existing.CreatedAt
	} else if row.CreatedAt == "" {
		row.CreatedAt = ts
	}
	row.UpdatedAt = ts
	row.IntervalSec = normalizeIntervalSec(row.IntervalSec)

	_, err := s.db.Exec(`
		INSERT INTO services (
		  service_id, name, runtime_dir, runtime_dirs, health_url, port,
		  start_cmd, stop_cmd, restart_cmd, watchdog_enabled, interval_sec,
		  restart_notify_url, restart_poll_url, graceful_max_wait_ms,
		  git_repo_url, default_branch,
		  created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(service_id) DO UPDATE SET
		  name = excluded.name,
		  runtime_dir = excluded.runtime_dir,
		  runtime_dirs = excluded.runtime_dirs,
		  health_url = excluded.health_url,
		  port = excluded.port,
		  start_cmd = excluded.start_cmd,
		  stop_cmd = excluded.stop_cmd,
		  restart_cmd = excluded.restart_cmd,
		  watchdog_enabled = excluded.watchdog_enabled,
		  interval_sec = excluded.interval_sec,
		  restart_notify_url = excluded.restart_notify_url,
		  restart_poll_url = excluded.restart_poll_url,
		  graceful_max_wait_ms = excluded.graceful_max_wait_ms,
		  git_repo_url = excluded.git_repo_url,
		  default_branch = excluded.default_branch,
		  updated_at = excluded.updated_at`,
		row.ServiceID, row.Name, row.RuntimeDir, encodeRuntimeDirs(row.RuntimeDirs),
		row.HealthURL, normalizePort(row.Port),
		row.StartCmd, row.StopCmd, row.RestartCmd, boolToInt(row.Supervise),
		row.IntervalSec,
		row.RestartNotifyURL, row.RestartPollURL, row.GracefulMaxWaitMs,
		row.GitRepoURL, defaultBranchOrMain(row.DefaultBranch),
		row.CreatedAt, row.UpdatedAt,
	)
	return row, err
}

// normalizePort 把非法端口收敛成 0（未设置）：写入库里的只有 0 或 1..65535。
func normalizePort(p int) int {
	if p < 0 || p > 65535 {
		return 0
	}
	return p
}

func (s *Store) GetService(serviceID string) (*ServiceContract, error) {
	row := s.db.QueryRow(`
		SELECT service_id, name, runtime_dir, runtime_dirs, health_url, port,
		       start_cmd, stop_cmd, restart_cmd, watchdog_enabled, interval_sec,
		       restart_notify_url, restart_poll_url, graceful_max_wait_ms,
		       git_repo_url, default_branch,
		       created_at, updated_at
		FROM services WHERE service_id = ?`, serviceID)
	svc, err := scanService(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return svc, err
}

// NormalizeServiceURLs 把库里所有契约的 URL 归一成「路径」形态（幂等）：老库里存的是
// http://127.0.0.1:4211/health，现在只存 /health，端口落进 port 列。返回改动条数。
// 外部端点（非 loopback / https）原样保留。
func (s *Store) NormalizeServiceURLs() (int, error) {
	svcs, err := s.ListServices()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, svc := range svcs {
		row := svc
		// 先补端口（老契约的端口只在 URL 里），再拆 URL —— 顺序不能反。
		changed := adoptURLPorts(&row)
		if NormalizeServiceContract(&row) {
			changed = true
		}
		if !changed {
			continue
		}
		row.CreatedAt = svc.CreatedAt
		if _, err := s.UpsertService(row); err != nil {
			return n, fmt.Errorf("normalize service urls %s: %w", svc.ServiceID, err)
		}
		n++
		fmt.Printf("[store] normalized service urls: %s health=%q port=%d\n", svc.ServiceID, row.HealthURL, row.Port)
	}
	return n, nil
}

func (s *Store) ListServices() ([]ServiceContract, error) {
	rows, err := s.db.Query(`
		SELECT service_id, name, runtime_dir, runtime_dirs, health_url, port,
		       start_cmd, stop_cmd, restart_cmd, watchdog_enabled, interval_sec,
		       restart_notify_url, restart_poll_url, graceful_max_wait_ms,
		       git_repo_url, default_branch,
		       created_at, updated_at
		FROM services ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ServiceContract
	for rows.Next() {
		svc, err := scanServiceRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *svc)
	}
	if out == nil {
		out = []ServiceContract{}
	}
	return out, rows.Err()
}

func (s *Store) DeleteService(serviceID string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM services WHERE service_id = ?`, serviceID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// deployColumns is the canonical SELECT list for deploys rows (shared by every
// query so a new column is added in one place).
const deployColumns = `request_id, service_id, deployment, target_machine, state, requested_at,
	started_at, finished_at, version, error, message,
	triggered_by_role, triggered_by_id`

// CreateDeploy records a queued deploy. targetMachine is optional (variadic so
// existing call sites stay unchanged): the first value is the deploy machine;
// empty/omitted = the default.
func (s *Store) CreateDeploy(requestID, serviceID, deployment string, by Identity, message string, targetMachine ...string) (DeployJob, error) {
	requestedAt := nowISO()
	job := DeployJob{
		RequestID:     requestID,
		ServiceID:     serviceID,
		Deployment:    deployment,
		TargetMachine: firstNonEmptyArg(targetMachine),
		State:         StateQueued,
		RequestedAt:   requestedAt,
		Message:       message,
	}
	job.setIdentity(by)
	_, err := s.db.Exec(`
		INSERT INTO deploys (
		  request_id, service_id, deployment, target_machine, state, requested_at, message,
		  triggered_by_role, triggered_by_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		job.RequestID, job.ServiceID, job.Deployment, job.TargetMachine, string(job.State), job.RequestedAt,
		nullIfEmpty(message), job.TriggeredByRole, job.TriggeredByID,
	)
	return job, err
}

func (s *Store) GetDeploy(requestID string) (*DeployJob, error) {
	row := s.db.QueryRow(`
		SELECT `+deployColumns+`
		FROM deploys WHERE request_id = ?`, requestID)
	job, err := scanDeploy(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return job, err
}

// ListDeploys returns the most recent deploys (no filters). Kept as a thin
// wrapper over ListDeploysFiltered for callers that only need a limit.
func (s *Store) ListDeploys(limit int) ([]DeployJob, error) {
	jobs, _, err := s.ListDeploysFiltered(ListFilter{Page: 1, PageSize: limit})
	return jobs, err
}

func (s *Store) ListDeploysByState(state DeployState) ([]DeployJob, error) {
	rows, err := s.db.Query(`
		SELECT `+deployColumns+`
		FROM deploys WHERE state = ? ORDER BY requested_at ASC`, string(state))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeployJob
	for rows.Next() {
		job, err := scanDeployRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *job)
	}
	if out == nil {
		out = []DeployJob{}
	}
	return out, rows.Err()
}

func (s *Store) ClaimNextQueued() (*DeployJob, error) {
	row := s.db.QueryRow(`SELECT request_id FROM deploys WHERE state = 'queued' ORDER BY requested_at ASC LIMIT 1`)
	var requestID string
	if err := row.Scan(&requestID); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return s.ClaimQueued(requestID)
}

// ClaimQueued atomically moves one specific queued deploy to `running`.
// Returns (nil, nil) when the row is no longer queued (already claimed by
// another tick/process) — callers must treat that as "somebody else took it".
// The conditional UPDATE is the CAS that keeps concurrent claims safe.
func (s *Store) ClaimQueued(requestID string) (*DeployJob, error) {
	res, err := s.db.Exec(
		`UPDATE deploys SET state = 'running', started_at = ? WHERE request_id = ? AND state = 'queued'`,
		nowISO(), requestID,
	)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, nil
	}
	return s.GetDeploy(requestID)
}

// QueuedDeploys returns every deploy still waiting to be claimed, oldest first.
// The worker uses the whole list (not just the head) so it can pick the oldest
// entry that is *runnable* instead of blocking on one that is not.
func (s *Store) QueuedDeploys() ([]DeployJob, error) {
	rows, err := s.db.Query("SELECT " + deployColumns + " FROM deploys WHERE state = 'queued' ORDER BY requested_at ASC, request_id ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeployJob
	for rows.Next() {
		job, err := scanDeployRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *job)
	}
	if out == nil {
		out = []DeployJob{}
	}
	return out, rows.Err()
}

type FinishPatch struct {
	State   DeployState
	Version string
	Error   string
	Message string
}

func (s *Store) FinishDeploy(requestID string, patch FinishPatch) (*DeployJob, error) {
	_, err := s.db.Exec(`
		UPDATE deploys SET
		  state = ?,
		  finished_at = ?,
		  version = COALESCE(?, version),
		  error = ?,
		  message = COALESCE(?, message)
		WHERE request_id = ?`,
		string(patch.State),
		nowISO(),
		nullIfEmpty(patch.Version),
		nullIfEmpty(patch.Error),
		nullIfEmpty(patch.Message),
		requestID,
	)
	if err != nil {
		return nil, err
	}
	return s.GetDeploy(requestID)
}

type scannable interface {
	Scan(dest ...any) error
}

func scanService(row scannable) (*ServiceContract, error) {
	var svc ServiceContract
	var watchdog int
	var runtimeDirs string
	err := row.Scan(
		&svc.ServiceID, &svc.Name, &svc.RuntimeDir, &runtimeDirs, &svc.HealthURL, &svc.Port,
		&svc.StartCmd, &svc.StopCmd, &svc.RestartCmd, &watchdog, &svc.IntervalSec,
		&svc.RestartNotifyURL, &svc.RestartPollURL, &svc.GracefulMaxWaitMs,
		&svc.GitRepoURL, &svc.DefaultBranch,
		&svc.CreatedAt, &svc.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	svc.DefaultBranch = defaultBranchOrMain(svc.DefaultBranch)
	svc.RuntimeDirs = decodeRuntimeDirs(runtimeDirs)
	svc.Supervise = watchdog != 0
	svc.IntervalSec = normalizeIntervalSec(svc.IntervalSec)
	return &svc, nil
}

func normalizeIntervalSec(n int) int {
	if n < minProbeIntervalSec {
		return defaultProbeIntervalSec
	}
	if n > maxProbeIntervalSec {
		return maxProbeIntervalSec
	}
	return n
}

// migrateSuperviseDefaultsOnce flips the leftover watchdog_enabled=0 rows for
// the control-plane first wave. Later operator unchecks are kept.
func (s *Store) migrateSuperviseDefaultsOnce() error {
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS meta (
		  k TEXT PRIMARY KEY,
		  v TEXT NOT NULL
		)`); err != nil {
		return err
	}
	var existing string
	err := s.db.QueryRow(`SELECT v FROM meta WHERE k = 'supervise_v1'`).Scan(&existing)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	placeholders := make([]string, len(defaultSuperviseServiceIDs))
	args := make([]any, len(defaultSuperviseServiceIDs))
	for i, id := range defaultSuperviseServiceIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	_, err = s.db.Exec(
		`UPDATE services SET watchdog_enabled = 1 WHERE service_id IN (`+
			strings.Join(placeholders, ",")+`)`,
		args...,
	)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO meta (k, v) VALUES ('supervise_v1', ?)`, nowISO())
	return err
}

func defaultBranchOrMain(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "main"
	}
	return s
}

func scanServiceRows(rows *sql.Rows) (*ServiceContract, error) {
	return scanService(rows)
}

func scanDeploy(row scannable) (*DeployJob, error) {
	var job DeployJob
	var started, finished, version, errStr, message sql.NullString
	var state string
	err := row.Scan(
		&job.RequestID, &job.ServiceID, &job.Deployment, &job.TargetMachine, &state, &job.RequestedAt,
		&started, &finished, &version, &errStr, &message,
		&job.TriggeredByRole, &job.TriggeredByID,
	)
	if err != nil {
		return nil, err
	}
	job.State = DeployState(state)
	job.StartedAt = started.String
	job.FinishedAt = finished.String
	job.Version = version.String
	job.Error = errStr.String
	job.Message = message.String
	job.TriggeredBy = job.Identity().String()
	return &job, nil
}

func scanDeployRows(rows *sql.Rows) (*DeployJob, error) {
	return scanDeploy(rows)
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// firstNonEmptyArg returns the first non-empty (trimmed) value, else "". Used
// for the optional variadic machine argument on CreatePipeline/CreateDeploy.
func firstNonEmptyArg(vals []string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// boolToInt stores a Go bool in a SQLite INTEGER column (0/1).
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
