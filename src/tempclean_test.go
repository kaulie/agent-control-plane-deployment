package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// makeReadOnlyTree 造一棵"含只读 Go 模块缓存"的树，模拟 build.sh 在构建树里留下的
// GOCACHE/GOMODCACHE（0555 目录 + 0444 文件）。
func makeReadOnlyTree(t *testing.T, root string) {
	t.Helper()
	cache := filepath.Join(root, "src", ".gomodcache", "pkg", "mod", "example@v1")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cache, "x.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(filepath.Join(cache, "x.go"), 0o444); err != nil {
		t.Fatalf("Chmod file: %v", err)
	}
	for _, d := range []string{cache, filepath.Dir(cache), filepath.Dir(filepath.Dir(cache)),
		filepath.Join(root, "src", ".gomodcache"), filepath.Join(root, "src")} {
		if err := os.Chmod(d, 0o555); err != nil {
			t.Fatalf("Chmod dir %s: %v", d, err)
		}
	}
}

// TestRemoveAllForceRemovesReadOnlyTree 是这次问题的核心回归：只读缓存会让
// os.RemoveAll 失败、残留几百 MB，removeAllForce 必须先放开写权限再删干净。
func TestRemoveAllForceRemovesReadOnlyTree(t *testing.T) {
	root := filepath.Join(t.TempDir(), "release-acp-12345")
	makeReadOnlyTree(t, root)

	if err := removeAllForce(root); err != nil {
		t.Fatalf("removeAllForce: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("tree still exists after removeAllForce (stat err = %v)", err)
	}

	// 已经不在了也不能报错（并发/重复清理是正常情况）。
	if err := removeAllForce(root); err != nil {
		t.Fatalf("removeAllForce on a missing path: %v", err)
	}
	if err := removeAllForce(""); err != nil {
		t.Fatalf("removeAllForce(\"\") = %v, want nil", err)
	}
}

// TestOSRemoveAllFailsOnReadOnlyTree 记录"为什么需要 force 版"：普通 RemoveAll
// 在这棵树上会失败。若某个平台/文件系统上它恰好能成功，就跳过该断言。
func TestOSRemoveAllFailsOnReadOnlyTree(t *testing.T) {
	root := filepath.Join(t.TempDir(), "release-acp-99999")
	makeReadOnlyTree(t, root)
	if err := os.RemoveAll(root); err == nil {
		t.Skip("os.RemoveAll handled the read-only tree on this platform; force version is still correct")
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("expected leftovers after a failed RemoveAll, stat: %v", err)
	}
	if err := removeAllForce(root); err != nil {
		t.Fatalf("removeAllForce after failed RemoveAll: %v", err)
	}
}

func TestCleanupStaleTempWork(t *testing.T) {
	tmp := t.TempDir()
	stamp := time.Now().UnixNano()
	stale := filepath.Join(tmp, tempPrefixReleaseBuild+itoa(stamp))
	fresh := filepath.Join(tmp, tempPrefixDeployPkg+itoa(stamp))
	other := filepath.Join(tmp, "not-ours-"+itoa(stamp))
	for _, d := range []string{stale, fresh, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", d, err)
		}
	}
	makeReadOnlyTree(t, stale) // 残留目录往往是这种"含只读缓存"的树
	old := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// 只清"超过 1 小时没动过"的；新的（可能正在跑）与不是我们前缀的都留着。
	removed, freed, failed := cleanupStaleTempWorkIn(tmp, time.Hour)
	if failed != 0 {
		t.Fatalf("cleanupStaleTempWorkIn failed on %d dir(s)", failed)
	}
	if removed != 1 || freed <= 0 {
		t.Fatalf("cleanupStaleTempWorkIn = (%d, %d), want 1 dir with freed bytes", removed, freed)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale work dir should be gone (stat err = %v)", err)
	}
	for _, d := range []string{fresh, other} {
		if _, err := os.Stat(d); err != nil {
			t.Fatalf("%s must be kept: %v", d, err)
		}
	}

	// 启动时（maxAge<=0）：全部清掉（此刻不可能有本进程的构建在跑）。
	removed, _, failed = cleanupStaleTempWorkIn(tmp, 0)
	if removed != 1 || failed != 0 {
		t.Fatalf("startup sweep = (%d, %d), want (1, 0)", removed, failed)
	}
	if _, err := os.Stat(fresh); !os.IsNotExist(err) {
		t.Fatalf("startup sweep should remove the fresh leftover too (stat err = %v)", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("unrelated dir must be kept: %v", err)
	}
}

func TestPruneUpgradeRequests(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "upgrade-requests")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	write := func(name string, age time.Duration, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
		ts := time.Now().Add(-age)
		if err := os.Chtimes(p, ts, ts); err != nil {
			t.Fatalf("Chtimes %s: %v", name, err)
		}
		return p
	}
	oldDone := write("deploy-req-old.json.done", 30*24*time.Hour, `{"ok":true}`)
	freshDone := write("deploy-req-new.json.done", time.Hour, `{"ok":true}`)
	oldBad := write("deploy-req-old.json.bad", 30*24*time.Hour, `{"broken":`)
	oldFailed := write("deploy-req-old.json.failed", 30*24*time.Hour, "health check failed\n")
	pending := write("deploy-req-pending.json", 30*24*time.Hour, `{"requestId":"x"}`)

	removed, failed := pruneUpgradeRequests(home, upgradeRequestRetention)
	if removed != 3 || failed != 0 {
		t.Fatalf("pruneUpgradeRequests = (%d, %d), want (3, 0)", removed, failed)
	}
	for _, p := range []string{oldDone, oldBad, oldFailed} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s should be pruned (terminal state, past retention)", p)
		}
	}
	for _, p := range []string{freshDone, pending} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s must be kept: %v", p, err)
		}
	}
	// 待处理请求的内容不能被改动（upgrader 还要读）。
	raw, err := os.ReadFile(pending)
	if err != nil {
		t.Fatalf("read pending: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil || body["requestId"] != "x" {
		t.Fatalf("pending request content changed: %s (%v)", raw, err)
	}
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}

// ourTempDirs 列出系统临时目录里属于本服务的工作目录（用于断言"没有新增残留"）。
func ourTempDirs(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatalf("ReadDir(TempDir): %v", err)
	}
	out := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() && hasTempWorkPrefix(e.Name()) {
			out[e.Name()] = true
		}
	}
	return out
}

// TestPackageFromGitCleansUpReadOnlyBuildTree 是"构建包没及时清理"的端到端回归：
// 真实 build.sh 会把 GOCACHE/GOMODCACHE 建在构建树里（只读），拿普通 os.RemoveAll
// 会 Permission denied、把 ~460MB 的构建树留在 /var/folders。这里跑一次真实的
// packageFromGit（本地 git 仓库 + 只读缓存的 build.sh），断言临时目录里没有新增残留。
func TestPackageFromGitCleansUpReadOnlyBuildTree(t *testing.T) {
	for _, bin := range []string{"git", "rsync", "tar"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}

	repo := t.TempDir()
	buildSh := `#!/usr/bin/env bash
set -euo pipefail
mkdir -p outputs/web
echo ok > outputs/web/index.html
# 模拟真实 build.sh：Go 构建/模块缓存建在构建树里，权限是只读的（0555/0444）
mkdir -p .gomodcache/pkg/mod/example@v1
echo "package x" > .gomodcache/pkg/mod/example@v1/f.go
chmod 444 .gomodcache/pkg/mod/example@v1/f.go
chmod 555 .gomodcache/pkg/mod/example@v1 .gomodcache/pkg/mod .gomodcache/pkg .gomodcache
`
	if err := os.WriteFile(filepath.Join(repo, "build.sh"), []byte(buildSh), 0o755); err != nil {
		t.Fatalf("WriteFile build.sh: %v", err)
	}
	runGit(t, repo, "init", "-q", "-b", "main")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "-qm", "init")

	before := ourTempDirs(t)
	storage := &localStorage{base: t.TempDir()}
	res, err := packageFromGit("svc-clean", "file://"+repo, "main", 120, storage, nil)
	if err != nil {
		t.Fatalf("packageFromGit: %v", err)
	}
	if res.Skipped {
		t.Fatalf("unexpected skip: %+v", res)
	}
	if res.Tag == "" || res.Hash == "" {
		t.Fatalf("package result incomplete: %+v", res)
	}
	for name := range ourTempDirs(t) {
		if !before[name] {
			t.Fatalf("构建后残留了临时工作目录：%s（清理没生效）", name)
		}
	}
	// 构建树本身也必须没了（不是只清掉一部分）。
	for name := range before {
		if !ourTempDirs(t)[name] {
			t.Fatalf("%s 被误删了（不该动别人的目录）", name)
		}
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
