package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 自部署收尾（reconcileOrphanDeploys）用契约里的**路径**+端口拼出探活地址。
// 回归：曾经这里直接拿 service.HealthURL（"/health"）当 URL 请求，导致自部署被判失败。
func TestReconcileProbesComposedHealthURL(t *testing.T) {
	hit := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit <- r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}
	port, _ := strconv.Atoi(u.Port())

	store, err := NewStore(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	runtimeDir := t.TempDir()
	hash := "abc12345"
	if err := os.WriteFile(filepath.Join(runtimeDir, "VERSION"), []byte(hash+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := ServiceContract{
		ServiceID:  "web-cursor",
		RuntimeDir: runtimeDir,
		HealthURL:  "/health", // 路径形态（新约定）
		Port:       port,
		StartCmd:   "true", StopCmd: "true", RestartCmd: "true",
	}
	if _, err := store.UpsertService(svc); err != nil {
		t.Fatalf("UpsertService: %v", err)
	}
	// 一条 running 的部署（模拟「自部署移交 upgrader 后、旧进程被杀」的现场）。
	if _, err := store.CreateDeploy("deploy-orphan1", "web-cursor", "deployment-"+hash, Identity{}, "running", ""); err != nil {
		t.Fatalf("CreateDeploy: %v", err)
	}
	if _, err := store.ClaimNextQueued(); err != nil {
		t.Fatalf("ClaimNextQueued: %v", err)
	}

	if n := reconcileOrphanDeploys(store); n != 1 {
		t.Fatalf("reconciled %d deploys, want 1", n)
	}

	job, err := store.GetDeploy("deploy-orphan1")
	if err != nil || job == nil {
		t.Fatalf("GetDeploy: %v %v", job, err)
	}
	if job.State != StateSucceeded {
		t.Fatalf("deploy = %s error=%q —— 探活必须用拼出来的地址（http://127.0.0.1:<port>/health）", job.State, job.Error)
	}
	select {
	case path := <-hit:
		if path != "/health" {
			t.Fatalf("probed path = %q, want /health", path)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reconcile must actually probe the health endpoint")
	}
}

// 健康检查真的不通时，错误信息里要出现**拼好的**地址（而不是裸路径）。
func TestReconcileHealthFailureNamesComposedURL(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()
	runtimeDir := t.TempDir()
	hash := "deadbeef"
	if err := os.WriteFile(filepath.Join(runtimeDir, "VERSION"), []byte(hash+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 端口 1 上没有服务：探活必定失败。
	if _, err := store.UpsertService(ServiceContract{
		ServiceID: "web-cursor", RuntimeDir: runtimeDir, HealthURL: "/health", Port: 1,
		StartCmd: "true", StopCmd: "true", RestartCmd: "true",
	}); err != nil {
		t.Fatalf("UpsertService: %v", err)
	}
	if _, err := store.CreateDeploy("deploy-orphan2", "web-cursor", "deployment-"+hash, Identity{}, "running", ""); err != nil {
		t.Fatalf("CreateDeploy: %v", err)
	}
	if _, err := store.ClaimNextQueued(); err != nil {
		t.Fatalf("ClaimNextQueued: %v", err)
	}
	reconcileOrphanDeploys(store)
	job, _ := store.GetDeploy("deploy-orphan2")
	if job == nil || job.State != StateFailed {
		t.Fatalf("deploy = %+v, want failed", job)
	}
	if !strings.Contains(job.Error, "http://127.0.0.1:1/health") {
		t.Fatalf("error should name the composed url, got %q", job.Error)
	}
}
