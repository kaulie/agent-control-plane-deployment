package main

import (
	"net/http"
	"path/filepath"
	"testing"
)

// 契约里的 URL 只存路径（host+port 部署时按目标机器拼，contract 与机器无关）。

func TestNormalizeServiceURL(t *testing.T) {
	cases := []struct {
		in         string
		wantStored string
		wantPort   int
	}{
		{"", "", 0},
		{"/health", "/health", 0},
		{"health", "/health", 0},
		{"/api/ops/restart-status?deep=1", "/api/ops/restart-status?deep=1", 0},
		// loopback 的完整 URL = 机器相关 → 拆成路径 + 端口
		{"http://127.0.0.1:4211/health", "/health", 4211},
		{"http://localhost:4300/api/ops/restart-notify", "/api/ops/restart-notify", 4300},
		{"http://127.0.0.1:4300", "/", 4300},
		// https / 外部主机 = 显式端点 → 原样保留
		{"https://127.0.0.1:4211/health", "https://127.0.0.1:4211/health", 0},
		{"https://api.example.com/health", "https://api.example.com/health", 0},
		{"http://10.0.0.7:4211/health", "http://10.0.0.7:4211/health", 0},
	}
	for _, c := range cases {
		stored, port := NormalizeServiceURL(c.in)
		if stored != c.wantStored || port != c.wantPort {
			t.Fatalf("NormalizeServiceURL(%q) = (%q, %d), want (%q, %d)", c.in, stored, port, c.wantStored, c.wantPort)
		}
	}
}

func TestComposeServiceURL(t *testing.T) {
	cases := []struct {
		stored string
		port   int
		want   string
	}{
		{"/health", 4211, "http://127.0.0.1:4211/health"},
		{"health", 4300, "http://127.0.0.1:4300/health"},
		{"/api/ops/restart-status", 4300, "http://127.0.0.1:4300/api/ops/restart-status"},
		// 外部端点原样返回（不拼本机 loopback）
		{"https://api.example.com/health", 4211, "https://api.example.com/health"},
		{"", 4211, ""},
		// 端口未知：原样（调用方另有告警）
		{"/health", 0, "/health"},
	}
	for _, c := range cases {
		if got := ComposeServiceURL(c.stored, c.port); got != c.want {
			t.Fatalf("ComposeServiceURL(%q, %d) = %q, want %q", c.stored, c.port, got, c.want)
		}
	}
}

func TestNormalizeServiceContractKeepsPort(t *testing.T) {
	c := ServiceContract{
		ServiceID:        "web-cursor",
		HealthURL:        "http://127.0.0.1:4211/health",
		RestartNotifyURL: "http://127.0.0.1:4211/api/ops/restart-notify",
		RestartPollURL:   "/api/ops/restart-status",
		Port:             4222,
	}
	if !NormalizeServiceContract(&c) {
		t.Fatal("normalizing a loopback URL must report a change")
	}
	if c.HealthURL != "/health" || c.RestartNotifyURL != "/api/ops/restart-notify" {
		t.Fatalf("URLs must be stored as paths, got %+v", c)
	}
	if c.Port != 4222 {
		t.Fatalf("an explicitly configured port must not be overwritten by the URL's port, got %d", c.Port)
	}
	if NormalizeServiceContract(&c) {
		t.Fatal("normalizing an already path-only contract must be a no-op")
	}
}

// 老库迁移：URL 里带 host+port、port 列为空 → 拆成路径 + 补端口；幂等。
func TestServiceURLMigrationFromFullURLs(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(filepath.Join(dir, "test.sqlite"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	// 直接写一条「老形态」的契约（绕过 UpsertService 的归一）：模拟升级前的库。
	if _, err := store.db.Exec(`
		INSERT INTO services (service_id, name, runtime_dir, health_url, port,
		  start_cmd, stop_cmd, restart_cmd, watchdog_enabled,
		  restart_notify_url, restart_poll_url, graceful_max_wait_ms,
		  git_repo_url, default_branch, created_at, updated_at)
		VALUES ('autonomy','autonomy','/tmp/autonomy','http://127.0.0.1:4300/health',0,
		  'true','true','bash scripts/restart.sh',0,
		  'http://127.0.0.1:4300/api/ops/restart-notify','http://127.0.0.1:4300/api/ops/restart-status',0,
		  'https://github.com/kaulie/autonomy','main','2026-09-01T00:00:00.000Z','2026-09-01T00:00:00.000Z')`); err != nil {
		t.Fatalf("seed old row: %v", err)
	}

	n, err := store.NormalizeServiceURLs()
	if err != nil {
		t.Fatalf("NormalizeServiceURLs: %v", err)
	}
	if n != 1 {
		t.Fatalf("migrated %d contract(s), want 1", n)
	}
	svc, err := store.GetService("autonomy")
	if err != nil || svc == nil {
		t.Fatalf("GetService: %v %v", svc, err)
	}
	if svc.HealthURL != "/health" || svc.RestartNotifyURL != "/api/ops/restart-notify" || svc.RestartPollURL != "/api/ops/restart-status" {
		t.Fatalf("URLs must be paths after migration, got %+v", svc)
	}
	if svc.Port != 4300 {
		t.Fatalf("the port from the old URL must land in contract.Port, got %d", svc.Port)
	}
	// 拼出来的地址与老配置完全一致（迁移无损）。
	if got := serviceHealthURL(*svc); got != "http://127.0.0.1:4300/health" {
		t.Fatalf("composed health url = %q, want the old absolute url", got)
	}

	// 幂等：再跑一次没有改动。
	if n, err := store.NormalizeServiceURLs(); err != nil || n != 0 {
		t.Fatalf("second run = (%d, %v), want (0, nil)", n, err)
	}
}

// 写入契约时就把 URL 归一（PUT 存的是路径，响应里看到的也是路径）。
func TestPutServiceStoresPathOnlyURLs(t *testing.T) {
	srv, store := newTestIdentityServer(t, true)
	rec := serviceRequest(t, srv, http.MethodPut, "/api/services/web-cursor", `{
		"runtimeDir":"/tmp/web-cursor","port":4211,
		"healthUrl":"http://127.0.0.1:4211/health",
		"restartNotifyUrl":"http://127.0.0.1:4211/api/ops/restart-notify",
		"restartPollUrl":"http://127.0.0.1:4211/api/ops/restart-status",
		"startCmd":"true","stopCmd":"true","restartCmd":"true"}`)
	if rec.Code != 200 {
		t.Fatalf("PUT status = %d body=%s", rec.Code, rec.Body.String())
	}
	svc, err := store.GetService("web-cursor")
	if err != nil || svc == nil {
		t.Fatalf("GetService: %v %v", svc, err)
	}
	if svc.HealthURL != "/health" || svc.RestartNotifyURL != "/api/ops/restart-notify" {
		t.Fatalf("the stored contract must keep paths only, got %+v", svc)
	}
	if svc.Port != 4211 {
		t.Fatalf("port = %d, want 4211", svc.Port)
	}
}
