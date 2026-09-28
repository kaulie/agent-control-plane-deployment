package main

import (
	"database/sql"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// runtimeDir 支持按平台（mac/linux）配不同路径，部署时按目标机器平台选。

func TestRuntimeDirForOS(t *testing.T) {
	svc := ServiceContract{
		RuntimeDir:  "/Users/gaolei/runtime/web-cursor",
		RuntimeDirs: map[string]string{"linux": "/home/ubuntu/runtime/web-cursor"},
	}
	if got := svc.RuntimeDirForOS("darwin"); got != "/Users/gaolei/runtime/web-cursor" {
		t.Fatalf("darwin = %q, want the default", got)
	}
	if got := svc.RuntimeDirForOS("Linux"); got != "/home/ubuntu/runtime/web-cursor" {
		t.Fatalf("linux = %q, want the linux override", got)
	}
	// 平台名写法归一：macos/mac → darwin
	svc.RuntimeDirs = map[string]string{"macOS": "/tmp/mac-cursor", "linux": "/srv/web-cursor"}
	if got := svc.RuntimeDirForOS("darwin"); got != "/tmp/mac-cursor" {
		t.Fatalf("darwin (from macOS key) = %q", got)
	}
	if _, ok := svc.PlatformRuntimeDir("linux"); !ok {
		t.Fatal("linux must be an explicit platform entry")
	}
	if _, ok := svc.PlatformRuntimeDir("windows"); ok {
		t.Fatal("windows is not configured")
	}

	// 什么都没配 → 空（调用方报错）
	if got := (ServiceContract{}).RuntimeDirForOS("linux"); got != "" {
		t.Fatalf("no runtimeDir at all = %q, want empty", got)
	}
}

func TestRuntimeDirForRemotePrecedence(t *testing.T) {
	target := MachineTarget{ID: "43.162.117.240", Kind: "ssh", SSHHost: "agent-oversea", RuntimeHome: "/home/ubuntu/runtime"}
	localDefault := "/Users/gaolei/runtime/web-cursor"

	// ① 契约里该平台的显式路径最优先（用户说的「分平台路径」）
	svc := ServiceContract{RuntimeDir: localDefault, RuntimeDirs: map[string]string{"linux": "/opt/web-cursor"}}
	if got := svc.RuntimeDirForRemote("linux", "web-cursor", target); got != "/opt/web-cursor" {
		t.Fatalf("explicit linux dir must win, got %q", got)
	}
	// ② 没配该平台 → 用部署通道约定的 <remote-home>/<serviceId>
	svc = ServiceContract{RuntimeDir: localDefault}
	if got := svc.RuntimeDirForRemote("linux", "web-cursor", target); got != "/home/ubuntu/runtime/web-cursor" {
		t.Fatalf("channel convention must be used, got %q", got)
	}
	// ③ 通道也没给家目录 → 最后才回落默认（本机语义的路径，文档里说明这是兜底）
	svc = ServiceContract{RuntimeDir: localDefault}
	bare := MachineTarget{ID: "x", Kind: "ssh", SSHHost: "x"}
	if got := svc.RuntimeDirForRemote("linux", "web-cursor", bare); got != localDefault {
		t.Fatalf("last resort = %q, want the default", got)
	}
}

func TestRuntimeDirsStoreAndAPI(t *testing.T) {
	srv, store := newTestIdentityServer(t, true)
	rec := serviceRequest(t, srv, http.MethodPut, "/api/services/web-cursor", `{
		"runtimeDir":"/Users/gaolei/runtime/web-cursor",
		"runtimeDirs":{"macos":"/tmp/mac-cursor","Linux":"/home/ubuntu/runtime/web-cursor","windows":""},
		"healthUrl":"/health","port":4211,
		"startCmd":"true","stopCmd":"true","restartCmd":"true"}`)
	if rec.Code != 200 {
		t.Fatalf("PUT status = %d body=%s", rec.Code, rec.Body.String())
	}
	svc, err := store.GetService("web-cursor")
	if err != nil || svc == nil {
		t.Fatalf("GetService: %v %v", svc, err)
	}
	// 键归一（macos → darwin、Linux → linux），空值丢掉
	if len(svc.RuntimeDirs) != 2 || svc.RuntimeDirs["darwin"] != "/tmp/mac-cursor" || svc.RuntimeDirs["linux"] != "/home/ubuntu/runtime/web-cursor" {
		t.Fatalf("runtimeDirs = %v, want normalized darwin/linux", svc.RuntimeDirs)
	}
	// 列表接口也要带出来（面板要展示）
	if !strings.Contains(getJSON(t, srv, "/api/services").Body.String(), "/home/ubuntu/runtime/web-cursor") {
		t.Fatal("/api/services must expose runtimeDirs")
	}
}

// 老库（services 表没有 runtime_dirs 列）打开时自动补列。
func TestRuntimeDirsColumnMigration(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "old.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE services (
		  service_id TEXT PRIMARY KEY, name TEXT NOT NULL, runtime_dir TEXT NOT NULL,
		  health_url TEXT NOT NULL, port INTEGER NOT NULL DEFAULT 0,
		  start_cmd TEXT NOT NULL, stop_cmd TEXT NOT NULL, restart_cmd TEXT NOT NULL,
		  watchdog_enabled INTEGER NOT NULL DEFAULT 1, restart_notify_url TEXT NOT NULL DEFAULT '',
		  restart_poll_url TEXT NOT NULL DEFAULT '', graceful_max_wait_ms INTEGER NOT NULL DEFAULT 0,
		  git_repo_url TEXT NOT NULL DEFAULT '', default_branch TEXT NOT NULL DEFAULT 'main',
		  created_at TEXT NOT NULL, updated_at TEXT NOT NULL
		);
		INSERT INTO services (service_id, name, runtime_dir, health_url, port, start_cmd, stop_cmd,
		  restart_cmd, created_at, updated_at)
		VALUES ('web-cursor','Web Cursor','/Users/gaolei/runtime/web-cursor','/health',4211,
		  'true','true','true','2026-09-01T00:00:00.000Z','2026-09-01T00:00:00.000Z');`); err != nil {
		t.Fatalf("seed old schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore (migration): %v", err)
	}
	defer store.Close()
	svc, err := store.GetService("web-cursor")
	if err != nil || svc == nil {
		t.Fatalf("GetService: %v %v", svc, err)
	}
	if svc.RuntimeDirs != nil {
		t.Fatalf("old rows have no per-OS dirs, got %v", svc.RuntimeDirs)
	}
	if got := svc.RuntimeDirForOS("linux"); got != "/Users/gaolei/runtime/web-cursor" {
		t.Fatalf("without a per-OS entry the default is used, got %q", got)
	}
}
