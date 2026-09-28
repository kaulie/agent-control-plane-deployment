package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// 远端部署（B）：制品 rsync 过去、在那边 graceful/重启/探活，本机 runtime 不动。

// fakeRemote 记录远端调用并按 URL 给出应答（真实实现走 ssh/rsync/ssh+curl）。
// unameOS/unameArch：模拟测试机自己的 uname 口径（远端平台检查用）。
func unameOS() string {
	if runtime.GOOS == "darwin" {
		return "Darwin"
	}
	return "Linux"
}

func unameArch() string {
	if runtime.GOARCH == "amd64" {
		return "x86_64"
	}
	return runtime.GOARCH
}

type fakeRemote struct {
	mu     sync.Mutex
	runs   []string
	pushes []string
	http   []string
	fail   string // 非空 = 该关键字的调用返回失败（用于失败路径）
	// 预检里 uname 的回答（默认 linux/amd64，与测试包里的二进制平台一致）。
	remoteOS   string
	remoteArch string
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
	// 预检脚本尾部会问平台；其余命令回 ok。
	if strings.Contains(script, "uname -s") {
		return 0, "ok\n" + f.remoteOS + "\n" + f.remoteArch
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
			"local": {ID: "local", Kind: "local"},
			"10.0.0.7": {ID: "10.0.0.7", Kind: "ssh", SSHUser: "ubuntu", SSHHost: "10.0.0.7", RuntimeHome: "/home/ubuntu/runtime",
				// 这台机器自己有 restart 通道（例如 systemd 管理的服务）。
				RestartCmd: "sudo systemctl restart web-cursor-{machine}"},
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

	fake := &fakeRemote{remoteOS: unameOS(), remoteArch: unameArch()}
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
		!strings.Contains(joinedRuns, "SERVICE_PORT='4211'") {
		t.Fatalf("the restart must run on the remote host with the service env, got:\n%s", joinedRuns)
	}
	// 这台机器配了 restart= → 用它的命令（而不是契约里的 restartCmd）。
	if !strings.Contains(joinedRuns, "sudo systemctl restart web-cursor-10.0.0.7") {
		t.Fatalf("the per-machine restart= override must be used, got:\n%s", joinedRuns)
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

	eventsIdx := map[string]int{}
	if all, err := store.ListDeployEvents("deploy-remote1"); err == nil {
		for i, e := range all {
			if _, ok := eventsIdx["preflight"]; !ok && strings.Contains(e.Message, "远端预检") {
				eventsIdx["preflight"] = i
			}
			if _, ok := eventsIdx["download"]; !ok && strings.Contains(e.Message, "下载开始") {
				eventsIdx["download"] = i
			}
		}
	}
	if p, ok := eventsIdx["preflight"]; !ok || p >= eventsIdx["download"] {
		t.Fatalf("the preflight must come before the download, got %v", eventsIdx)
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
	// 预检在下载之前：事件里「远端预检」必须早于「下载开始」（否则会白下载几百 MB）。
	events, _ := store.ListDeployEvents("deploy-remote2")
	preflightAt, downloadAt := -1, -1
	for i, e := range events {
		if strings.Contains(e.Message, "远端预检") && preflightAt < 0 {
			preflightAt = i
		}
		if strings.Contains(e.Message, "下载开始") && downloadAt < 0 {
			downloadAt = i
		}
	}
	if preflightAt < 0 {
		t.Fatal("the remote preflight must be recorded on the timeline")
	}
	if downloadAt >= 0 && preflightAt > downloadAt {
		t.Fatalf("the remote preflight must run before the download (preflight@%d download@%d)", preflightAt, downloadAt)
	}
	if downloadAt >= 0 {
		t.Fatal("a failed preflight must not even start the download")
	}
	if _, err := os.Stat(localRuntimeDir); !os.IsNotExist(err) {
		t.Fatalf("a remote deploy must not touch the local runtime dir, stat err=%v", err)
	}
}

// ssh 认证失败（免密配在 ~/.ssh/config 别名上、却用字面 host 登录）要给可操作提示。
func TestRemotePreflightAuthFailureHintsAliasForm(t *testing.T) {
	store, _, _, _, _, _ := newRemoteDeployFixture(t)
	target := MachineTarget{ID: "43.162.117.240", Kind: "ssh", SSHUser: "ubuntu", SSHHost: "43.162.117.240", RuntimeHome: "/home/ubuntu/runtime"}
	job := DeployJob{RequestID: "deploy-auth1", TargetMachine: target.ID}
	fake := &authFailRemote{}
	code, out, _ := remotePreflight(store, job, target, fake, "/home/ubuntu/runtime/web-cursor")
	if code == 0 {
		t.Fatal("an auth failure must fail the preflight")
	}
	if !strings.Contains(out, "~/.ssh/config") || !strings.Contains(out, "ssh <别名>") {
		t.Fatalf("the failure must point at the alias form, got %q", out)
	}
}

type authFailRemote struct{}

func (authFailRemote) Run(target MachineTarget, script string, timeoutSec int) (int, string) {
	return 255, "ubuntu@43.162.117.240: Permission denied (publickey)."
}

func (authFailRemote) PushDir(target MachineTarget, src, dst string, deleteExtra bool, timeoutSec int) (int, string) {
	return 255, "Permission denied (publickey)."
}

func (authFailRemote) HTTP(target MachineTarget, method, rawURL, body string, timeoutSec int) (int, string) {
	return 0, "Permission denied (publickey)."
}
