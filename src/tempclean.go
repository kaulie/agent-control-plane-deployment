package main

import (
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 部署过程中的临时工作目录都建在系统临时目录（os.TempDir()）下，前缀如下：
//
//	release-acp-*  packageFromGit 的构建树（git fetch + build.sh，含 Go 构建/模块缓存）
//	release-pkg-*  打包暂存目录（交给制品存储后端上传）
//	deploy-pkg-*   部署时把制品下载到本地的目录
//
// 这些目录每次构建可以有几百 MB（build.sh 把 GOCACHE/GOMODCACHE 建在构建树里），
// 必须能被可靠地清掉，否则会在 /var/folders 里越积越多。
const (
	tempPrefixReleaseBuild = "release-acp-"
	tempPrefixReleasePkg   = "release-pkg-"
	tempPrefixDeployPkg    = "deploy-pkg-"

	// 已完成（*.json.done）的升级请求记录保留多久。
	upgradeRequestRetention = 7 * 24 * time.Hour
	// 运行期清理间隔 / 判定"残留"的最小年龄（正常构建最多几分钟）。
	tempWorkSweepInterval = 30 * time.Minute
	tempWorkStaleAfter    = time.Hour
)

var tempWorkPrefixes = []string{tempPrefixReleaseBuild, tempPrefixReleasePkg, tempPrefixDeployPkg}

// removeAllForce 删除临时工作目录，**包括里面只读的 Go 构建/模块缓存**。
//
// 为什么不能只用 os.RemoveAll：build.sh 把 GOCACHE/GOMODCACHE/GOPATH 建在构建树里
// （见 build.sh 的说明），Go 会把模块缓存设成 0555/0444；从只读目录里 unlink 子项
// 需要目录本身可写，于是 os.RemoveAll 会以 EACCES 失败、只删掉一部分（实测每次
// 构建残留 ~460MB，恰好剩 src/.gomodcache —— `rm -rf` 同样失败）。所以先把整棵树
// chmod u+w 再删，并且**把错误返回给调用方**，不再静默失败。
func removeAllForce(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	if _, err := os.Lstat(path); err != nil {
		return nil // 已经不在了
	}
	chmodWritableRecursive(path)
	if err := os.RemoveAll(path); err != nil {
		// 再试一次：遍历与删除之间可能又被写入了（例如还没退出的子进程）。
		chmodWritableRecursive(path)
		return os.RemoveAll(path)
	}
	return nil
}

// chmodWritableRecursive 尽力把整棵树改成可写（忽略单个失败：只读目录仍然可以遍历）。
func chmodWritableRecursive(root string) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			_ = os.Chmod(path, 0o755)
			return nil
		}
		_ = os.Chmod(path, 0o644)
		return nil
	})
}

// cleanupStaleTempWork 清理系统临时目录里属于本服务的残留工作目录。
func cleanupStaleTempWork(maxAge time.Duration) (removed int, freed int64, failed int) {
	return cleanupStaleTempWorkIn(os.TempDir(), maxAge)
}

// cleanupStaleTempWorkIn 是 cleanupStaleTempWork 的可指定目录版本（测试用）。
// 返回 (清掉的个数, 释放的字节数, 失败个数)。
//
// maxAge <= 0 表示"不看到期，全清"——进程刚启动时成立：此刻不可能有本进程的构建
// 在跑（被 kill 掉的上一次运行留下的目录必然是垃圾）。maxAge > 0 时只清改时间早于
// 该年龄的目录，避免误删并发/正在跑的构建。
func cleanupStaleTempWorkIn(dir string, maxAge time.Duration) (removed int, freed int64, failed int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0, 1
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		if !e.IsDir() || !hasTempWorkPrefix(e.Name()) {
			continue
		}
		if maxAge > 0 {
			info, err := e.Info()
			if err != nil || info.ModTime().After(cutoff) {
				continue
			}
		}
		full := filepath.Join(dir, e.Name())
		size := dirSize(full)
		if err := removeAllForce(full); err != nil {
			failed++
			continue
		}
		removed++
		freed += size
	}
	return removed, freed, failed
}

func hasTempWorkPrefix(name string) bool {
	for _, p := range tempWorkPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// upgradeRequestTerminalSuffixes：acp-upgrader 处理完（无论成败）会给请求改名，
// 这些后缀都表示"这条记录已经结束了"：
//
//	*.json.done    停旧 → 起新 → 探活通过
//	*.json.failed  失败原因（upgrader 写的 error 文本）
//	*.json.bad     upgrader 认不了的请求（原 JSON 被改名保留）
//
// 待处理（*.json）**绝不能碰** —— upgrader 还要读它。
var upgradeRequestTerminalSuffixes = []string{".json.done", ".json.failed", ".json.bad"}

// pruneUpgradeRequests 清理 acp-upgrader 已经处理完的请求记录（超过保留期的），
// 保留期内的留着方便回溯审计；待处理（*.json）永远不动。
func pruneUpgradeRequests(home string, keep time.Duration) (removed int, failed int) {
	dir := filepath.Join(home, "upgrade-requests")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0
	}
	cutoff := time.Now().Add(-keep)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !hasTerminalUpgradeSuffix(name) {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			failed++
			continue
		}
		removed++
	}
	return removed, failed
}

func hasTerminalUpgradeSuffix(name string) bool {
	for _, s := range upgradeRequestTerminalSuffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

// startTempWorkJanitor 运行期兜底：周期性清掉"早就结束、却被异常终止（进程被
// upgrader / kill 掉）留下"的临时工作目录。启动时的那次（maxAge<=0）是主力，
// 这里只是防止长期不重启的进程一直留着垃圾。
func startTempWorkJanitor(stop <-chan struct{}, interval, staleAfter time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if n, freed, failed := cleanupStaleTempWork(staleAfter); n > 0 || failed > 0 {
					log.Printf("[cleanup] swept %d stale temp work dir(s), reclaimed %s (failed=%d)",
						n, humanBytes(freed), failed)
				}
			}
		}
	}()
}
