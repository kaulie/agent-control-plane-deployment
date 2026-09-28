package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// 远端部署（B）：制品 rsync 过去、在那边 graceful/重启/探活，本机 runtime 不动。

// fakeRemote 记录远端调用并按 URL 给出应答（真实实现走 ssh/rsync/ssh+curl）。
type fakeRemote struct {
	mu     sync.Mutex
	runs   []string
	pushes []string
	http   []string
	fail   string // 非空 = 该关键字的调用返回失败（用于失败路径）
}

func (f *fakeRemote) record(dst *[]string, v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	*dst = append(*dst, v)
}

func (f *fakeRemote) Run(target MachineTarget, script string, timeoutSec int) (int, string) {
	f.record(&f.runs, script)
	if f.fail != "" && strings.Contains(script, f.fail) {
		return 3, "boom: " + f.fail
	}
	return 0, "ok"
}

func (f *fakeRemote) PushDir(target MachineTarget, src, dst string, deleteExtra bool, timeoutSec int) (int, string) {
	f.record(&f.pushes, src+" → "+target.SSHDest()+":"+dst)
	if f.fail != "" && strings.Contains(dst, f.fail) {
		return 23, "rsync failed: " + f.fail
	}
	return 0, ""
}

func (f *fakeRemote) HTTP(target MachineTarget, method, rawURL, body string, timeoutSec int) (int, string) {
	f.record(&f.http, method+" "+rawURL)
	switch {
	case strings.Contains(rawURL, "/restart/poll"):
		return 200, `{"canRestart":true}`
	case strings.Contains(rawURL, "/health"):
		return 200, `{"ok":true}`
	}
	return 200, `{}`
}

func (f *fakeRemote) calls() (runs, pushes, http []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.runs...), append([]string(nil), f.pushes...), append([]string(nil), f.http...)
}

// newRemoteDeployFixture 准备一次「部署到 10.0.0.7」的现场：本地制品 + 服务契约 + 排队中的部署。
func newRemoteDeployFixture(t *testing.T) (*Store, Config, ArtifactStorage, *MachineCatalog, string, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := NewStore(filepath.Join(dir, "test.sqlite"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	pkgs := filepath.Join(dir, "packages")
	hash := "abc12345"
	tag := "deployment-" + hash
	snap := filepath.Join(pkgs, "web-cursor", tag)
	if err := os.MkdirAll(filepath.Join(snap, "scripts"), 0o755); err != nil {
		t.Fatalf("mkdir package: %v", err)
	}
	files := map[string]string{
		"VERSION":            hash + "\n",
		"DEPLOYMENT":         tag + "\n",
		"scripts/restart.sh": "#!/usr/bin/env bash\nexit 0\n",
		"DEPLOY-MARKER":      "keep me\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(snap, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	cfg := Config{
		Home:                dir,
		PackagesDir:         pkgs,
		ArtifactStorageType: "local",
		DeployMaxSec:        30,
		ReleaseMaxSec:       30,
		HealthCheckTimeout:  3 * time.Second,
		DeployMachineTargets: map[string]MachineTarget{
			"local":    {ID: "local", Kind: "local"},
			"10.0.0.7": {ID: "10.0.0.7", Kind: "ssh", SSHUser: "ubuntu", SSHHost: "10.0.0.7", RuntimeHome: "/home/ubuntu/runtime"},
		},
	}
	storage, err := NewArtifactStorage(cfg)
	if err != nil {
		t.Fatalf("NewArtifactStorage: %v", err)
	}
	svc := ServiceContract{
		ServiceID:  "web-cursor",
		RuntimeDir: filepath.Join(dir, "runtime-web-cursor"),
		HealthURL:  "http://127.0.0.1:4211/health",
		Port:       4211,
		StartCmd:   "true",
		StopCmd:    "true",
		RestartCmd: "bash scripts/restart.sh",
		// graceful：远端部署时这两条要在远端问。
		RestartNotifyURL: "http://127.0.0.1:4211/api/ops/restart-notify",
		RestartPollURL:   "http://127.0.0.1:4211/api/ops/restart-status",
	}
	if _, err := store.UpsertService(svc); err != nil {
		t.Fatalf("UpsertService: %v", err)
	}
	return store, cfg, storage, NewMachineCatalog(cfg, nil), tag, svc.RuntimeDir
}

func TestExecuteDeployToRemoteMachine(t *testing.T) {
	store, cfg, storage, machines, tag, localRuntimeDir := newRemoteDeployFixture(t)
	if _, err := store.CreateDeploy("deploy-remote1", "web-cursor", tag, Identity{}, "queued", "10.0.0.7"); err != nil {
		t.Fatalf("CreateDeploy: %v", err)
	}
	if _, err := store.ClaimNextQueued(); err != nil {
		t.Fatalf("ClaimNextQueued: %v", err)
	}

	fake := &fakeRemote{}
	executeDeploy(store, cfg, storage, &GracefulDrain{}, machines, fake, "deploy-remote1")

	job, err := store.GetDeploy("deploy-remote1")
	if err != nil || job == nil {
		t.Fatalf("GetDeploy: %v %v", job, err)
	}
	if job.State != StateSucceeded {
		t.Fatalf("deploy state = %s error=%q，want succeeded", job.State, job.Error)
	}
	if job.Version != "abc12345" {
		t.Fatalf("version = %q, want abc12345", job.Version)
	}

	runs, pushes, http := fake.calls()
	joinedRuns := strings.Join(runs, "\n")
	// ① 预检：curl 在不在、runtime 目录像不像这个服务。
	if !strings.Contains(joinedRuns, "command -v curl") || !strings.Contains(joinedRuns, "/home/ubuntu/runtime/web-cursor") {
		t.Fatalf("preflight must check curl + the remote runtime dir, got:\n%s", joinedRuns)
	}
	// ② 制品推送到远端目录。
	if len(pushes) != 1 || !strings.Contains(pushes[0], "→ ubuntu@10.0.0.7:/home/ubuntu/runtime/web-cursor") {
		t.Fatalf("pushes = %v, want one rsync to the remote runtime dir", pushes)
	}
	// ③ 远端重启：cd 到远端目录 + 注入 SERVICE_PORT/DEPLOY_MACHINE + 跑契约的 restartCmd。
	if !strings.Contains(joinedRuns, "cd '/home/ubuntu/runtime/web-cursor'") ||
		!strings.Contains(joinedRuns, "DEPLOY_MACHINE='10.0.0.7'") ||
		!strings.Contains(joinedRuns, "SERVICE_PORT='4211'") ||
		!strings.Contains(joinedRuns, "bash scripts/restart.sh") {
		t.Fatalf("the restart must run on the remote host with the service env, got:\n%s", joinedRuns)
	}
	// ④ graceful 与健康检查都问到远端（用远端自己的 127.0.0.1 地址）。
	joinedHTTP := strings.Join(http, "\n")
	for _, want := range []string{"POST http://127.0.0.1:4211/api/ops/restart-notify", "GET http://127.0.0.1:4211/api/ops/restart-status", "GET http://127.0.0.1:4211/health"} {
		if !strings.Contains(joinedHTTP, want) {
			t.Fatalf("remote http calls = %v, want %q", http, want)
		}
	}

	// ⑤ 本机 runtime 一动不动（没有落任何东西）。
	if _, err := os.Stat(localRuntimeDir); !os.IsNotExist(err) {
		t.Fatalf("a remote deploy must not touch the local runtime dir (%s), stat err=%v", localRuntimeDir, err)
	}

	events, _ := store.ListDeployEvents("deploy-remote1")
	joined := ""
	for _, e := range events {
		joined += e.Message + "\n"
	}
	for _, want := range []string{"部署机器=10.0.0.7", "远端预检", "制品已 rsync 到远端 runtime", "远端执行", "部署成功：version=abc12345（远端 10.0.0.7"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("deploy timeline should contain %q, got:\n%s", want, joined)
		}
	}
}

func TestExecuteDeployRemotePreflightFailureFailsFast(t *testing.T) {
	store, cfg, storage, machines, tag, localRuntimeDir := newRemoteDeployFixture(t)
	if _, err := store.CreateDeploy("deploy-remote2", "web-cursor", tag, Identity{}, "queued", "10.0.0.7"); err != nil {
		t.Fatalf("CreateDeploy: %v", err)
	}
	if _, err := store.ClaimNextQueued(); err != nil {
		t.Fatalf("ClaimNextQueued: %v", err)
	}
	// 远端预检失败（例如没有 curl）→ 部署失败，而且**不推制品**。
	fake := &fakeRemote{fail: "command -v curl"}
	executeDeploy(store, cfg, storage, &GracefulDrain{}, machines, fake, "deploy-remote2")

	job, _ := store.GetDeploy("deploy-remote2")
	if job == nil || job.State != StateFailed {
		t.Fatalf("preflight failure must fail the deploy, got %+v", job)
	}
	if !strings.Contains(job.Error, "remote preflight failed") {
		t.Fatalf("error should name the preflight, got %q", job.Error)
	}
	_, pushes, _ := fake.calls()
	if len(pushes) != 0 {
		t.Fatalf("nothing may be pushed when the preflight fails, got %v", pushes)
	}
	if _, err := os.Stat(localRuntimeDir); !os.IsNotExist(err) {
		t.Fatalf("a remote deploy must not touch the local runtime dir, stat err=%v", err)
	}
}
