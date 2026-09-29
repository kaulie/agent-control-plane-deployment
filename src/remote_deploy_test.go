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
	// failContains 更细：匹配到这段脚本时返回 failCode/failOutput（首次部署 restart 兜底用）。
	failContains string
	failCode     int
	failOutput   string
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
	if f.failContains != "" && strings.Contains(script, f.failContains) {
		code := f.failCode
		if code == 0 {
			code = 1
		}
		out := f.failOutput
		if out == "" {
			out = "boom: " + f.failContains
		}
		return code, out
	}
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
	// 远端部署 = 另一台机器（Linux）的产物：tag 带平台维度，与打包侧一致。
	tag := deploymentTagFor(hash, BuildPlatform{OS: "linux", Arch: "amd64"})
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

	fake := &fakeRemote{remoteOS: "Linux", remoteArch: "x86_64"}
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

// 契约里按平台配了路径（runtimeDirs.linux）时，远端部署用**它**，而不是通道约定的
// <remote-home>/<serviceId> —— 这就是「runtimeDir 按 mac/linux 两套路径」的落地。
func TestExecuteDeployUsesPlatformRuntimeDir(t *testing.T) {
	store, cfg, storage, machines, tag, _ := newRemoteDeployFixture(t)
	svc, err := store.GetService("web-cursor")
	if err != nil || svc == nil {
		t.Fatalf("GetService: %v %v", svc, err)
	}
	svc.RuntimeDirs = map[string]string{"linux": "/opt/web-cursor"}
	if _, err := store.UpsertService(*svc); err != nil {
		t.Fatalf("UpsertService: %v", err)
	}
	if _, err := store.CreateDeploy("deploy-remote3", "web-cursor", tag, Identity{}, "queued", "10.0.0.7"); err != nil {
		t.Fatalf("CreateDeploy: %v", err)
	}
	if _, err := store.ClaimNextQueued(); err != nil {
		t.Fatalf("ClaimNextQueued: %v", err)
	}

	// 假远端就是 Linux：会选 runtimeDirs.linux、且 tag 的平台与它一致。
	fake := &fakeRemote{remoteOS: "Linux", remoteArch: "x86_64"}
	executeDeploy(store, cfg, storage, &GracefulDrain{}, machines, fake, "deploy-remote3")

	job, _ := store.GetDeploy("deploy-remote3")
	if job == nil || job.State != StateSucceeded {
		t.Fatalf("deploy = %+v, want succeeded", job)
	}
	_, pushes, _ := fake.calls()
	if len(pushes) != 1 || !strings.Contains(pushes[0], ":/opt/web-cursor") {
		t.Fatalf("rsync must target the contract's linux runtimeDir, got %v", pushes)
	}
	runs, _, _ := fake.calls()
	joined := strings.Join(runs, "\n")
	if !strings.Contains(joined, "'/opt/web-cursor'") {
		t.Fatalf("the remote restart must run in the platform runtimeDir, got:\n%s", joined)
	}
}

// 本机平台的产物（老 tag，没有平台后缀）发到 Linux 远端：**在下载之前**就失败，
// 不下载、不推送（这正是「macOS 包发到 Linux 远端」那次的教训）。
func TestExecuteDeployRejectsForeignPlatformTagBeforeDownload(t *testing.T) {
	store, cfg, storage, machines, _, localRuntimeDir := newRemoteDeployFixture(t)
	svc, err := store.GetService("web-cursor")
	if err != nil || svc == nil {
		t.Fatalf("GetService: %v %v", svc, err)
	}
	_ = svc
	// 故意用「本机平台」的 tag（没有平台后缀）。
	hostTag := deploymentTagFor("abc12345", LocalBuildPlatform())
	if hostTag != "deployment-abc12345" {
		t.Fatalf("host tag = %q, want the legacy unqualified form", hostTag)
	}
	if _, err := store.CreateDeploy("deploy-remote4", "web-cursor", hostTag, Identity{}, "queued", "10.0.0.7"); err != nil {
		t.Fatalf("CreateDeploy: %v", err)
	}
	if _, err := store.ClaimNextQueued(); err != nil {
		t.Fatalf("ClaimNextQueued: %v", err)
	}

	fake := &fakeRemote{remoteOS: "Linux", remoteArch: "x86_64"}
	executeDeploy(store, cfg, storage, &GracefulDrain{}, machines, fake, "deploy-remote4")

	job, _ := store.GetDeploy("deploy-remote4")
	if job == nil || job.State != StateFailed {
		t.Fatalf("deploy = %+v, want failed", job)
	}
	if !strings.Contains(job.Error, "制品平台与机器不符") {
		t.Fatalf("error must name the platform mismatch, got %q", job.Error)
	}
	runs, pushes, _ := fake.calls()
	if len(pushes) != 0 {
		t.Fatalf("nothing may be pushed, got %v", pushes)
	}
	// 远端平台要先探测（平台检查需要它），但「目录预检 / 下载 / 推送」都不该发生。
	probed := false
	for _, r := range runs {
		if strings.Contains(r, "uname -s") {
			probed = true
		}
		if strings.Contains(r, "scripts/restart.sh") || strings.Contains(r, "mkdir -p") {
			t.Fatalf("the platform check must fire before the dir check/download:\n%s", r)
		}
	}
	if !probed {
		t.Fatal("the remote platform must be probed to compare with the tag")
	}
	events, _ := store.ListDeployEvents("deploy-remote4")
	for _, e := range events {
		if strings.Contains(e.Message, "下载开始") {
			t.Fatalf("the foreign-platform tag must not even download: %s", e.Message)
		}
	}
	_ = localRuntimeDir
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
	_, err := remoteProbe(store, job, target, fake)
	if err == nil {
		t.Fatal("an auth failure must fail the probe")
	}
	if !strings.Contains(err.Error(), "~/.ssh/config") || !strings.Contains(err.Error(), "ssh <别名>") {
		t.Fatalf("the failure must point at the alias form, got %v", err)
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

// 新机器第一次部署：restart.sh 的 stop 报「没有运行中的 Brain」并非 0 退出。
// 平台必须忽略这类错误、改跑 startCmd，部署才能成功。
func TestExecuteDeployFirstDeployIgnoresNotRunningRestart(t *testing.T) {
	store, cfg, storage, _, tag, _ := newRemoteDeployFixture(t)
	cfg.DeployMachineTargets["10.0.0.8"] = MachineTarget{
		ID: "10.0.0.8", Kind: "ssh", SSHUser: "ubuntu", SSHHost: "10.0.0.8",
		RuntimeHome: "/home/ubuntu/runtime",
	}
	machines := NewMachineCatalog(cfg, nil)
	svc, err := store.GetService("web-cursor")
	if err != nil || svc == nil {
		t.Fatalf("GetService: %v %v", svc, err)
	}
	svc.StartCmd = "bash scripts/start.sh"
	svc.RestartCmd = "bash scripts/restart.sh"
	if _, err := store.UpsertService(*svc); err != nil {
		t.Fatalf("UpsertService: %v", err)
	}
	if _, err := store.CreateDeploy("deploy-first1", "web-cursor", tag, Identity{}, "queued", "10.0.0.8"); err != nil {
		t.Fatalf("CreateDeploy: %v", err)
	}
	if _, err := store.ClaimNextQueued(); err != nil {
		t.Fatalf("ClaimNextQueued: %v", err)
	}

	fake := &fakeRemote{
		remoteOS:     "Linux",
		remoteArch:   "x86_64",
		failContains: "bash scripts/restart.sh",
		failCode:     1,
		failOutput:   "[restart] stop\n[stop] 没有运行中的 Brain\n[restart]\n",
	}
	executeDeploy(store, cfg, storage, &GracefulDrain{}, machines, fake, "deploy-first1")

	job, _ := store.GetDeploy("deploy-first1")
	if job == nil || job.State != StateSucceeded {
		t.Fatalf("first deploy must succeed when stop only said not-running, got %+v", job)
	}
	runs, _, _ := fake.calls()
	joined := strings.Join(runs, "\n---\n")
	if !strings.Contains(joined, "bash scripts/restart.sh") {
		t.Fatalf("must try restartCmd first, got:\n%s", joined)
	}
	if !strings.Contains(joined, "bash scripts/start.sh") {
		t.Fatalf("must fall back to startCmd, got:\n%s", joined)
	}
	events, _ := store.ListDeployEvents("deploy-first1")
	ev := ""
	for _, e := range events {
		ev += e.Message + "\n"
	}
	if !strings.Contains(ev, "restart 报服务未在运行") {
		t.Fatalf("timeline must record the not-running fallback, got:\n%s", ev)
	}
}

func TestExecuteDeployRestartCrashDoesNotFallbackToStart(t *testing.T) {
	store, cfg, storage, _, tag, _ := newRemoteDeployFixture(t)
	cfg.DeployMachineTargets["10.0.0.8"] = MachineTarget{
		ID: "10.0.0.8", Kind: "ssh", SSHUser: "ubuntu", SSHHost: "10.0.0.8",
		RuntimeHome: "/home/ubuntu/runtime",
	}
	machines := NewMachineCatalog(cfg, nil)
	svc, _ := store.GetService("web-cursor")
	svc.StartCmd = "bash scripts/start.sh"
	svc.RestartCmd = "bash scripts/restart.sh"
	if _, err := store.UpsertService(*svc); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateDeploy("deploy-crash1", "web-cursor", tag, Identity{}, "queued", "10.0.0.8"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimNextQueued(); err != nil {
		t.Fatal(err)
	}
	fake := &fakeRemote{
		remoteOS:     "Linux",
		remoteArch:   "x86_64",
		failContains: "bash scripts/restart.sh",
		failCode:     139,
		failOutput:   "segmentation fault",
	}
	executeDeploy(store, cfg, storage, &GracefulDrain{}, machines, fake, "deploy-crash1")

	job, _ := store.GetDeploy("deploy-crash1")
	if job == nil || job.State != StateFailed {
		t.Fatalf("a real restart crash must fail the deploy, got %+v", job)
	}
	if !strings.Contains(job.Error, "segmentation fault") {
		t.Fatalf("error must keep the restart failure, got %q", job.Error)
	}
	runs, _, _ := fake.calls()
	for _, r := range runs {
		if strings.Contains(r, "bash scripts/start.sh") {
			t.Fatalf("must not fall back to startCmd on a real crash, got %q", r)
		}
	}
}
