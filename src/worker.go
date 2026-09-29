package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kaulie/agent-control-plane-deployment/eventlevel"
)

func normalizeDeploymentTag(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("deployment is required")
	}
	if strings.HasPrefix(s, "deployment-") {
		return s, nil
	}
	return "deployment-" + strings.TrimPrefix(s, "deployment-"), nil
}

// assertRelease resolves a deployment tag and verifies that the artifact for
// that tag exists in the configured storage (local disk, GitHub Releases or
// Aliyun packages, etc.). The storage backend is the single source of truth
// for the bytes. serviceID is passed through to the backend because some
// backends (local disk, Aliyun) scope packages by service.
func assertRelease(storage ArtifactStorage, serviceID, gitRepoURL, deployment string) (string, error) {
	tag, err := normalizeDeploymentTag(deployment)
	if err != nil {
		return "", err
	}
	exists, err := storage.Exists(context.Background(), serviceID, gitRepoURL, tag)
	if err != nil {
		return "", fmt.Errorf("check artifact %s: %w", tag, err)
	}
	if !exists {
		return "", fmt.Errorf("artifact not found for tag %s on %s", tag, gitRepoURL)
	}
	return tag, nil
}

type shellResult struct {
	Code   int
	Output string
}

// runServiceCmd 在目标机器的 runtime 目录里跑契约命令（start/stop/restart），
// 注入与 restartCmd 相同的 SERVICE_PORT / PORT / APP_VERSION / DEPLOY_MACHINE / RUNTIME_DIR。
// restartOverride 只给「通道 restart=」用；跑 startCmd 兜底时必须留空。
func runServiceCmd(remote RemoteRunner, target MachineTarget, cmd string, service ServiceContract,
	runtimeDir, hash, machine string, timeoutSec int, restartOverride string) (int, string) {
	extra := map[string]string{"APP_VERSION": hash, "DEPLOY_MACHINE": machine, "RUNTIME_DIR": runtimeDir}
	if target.Remote() {
		remoteCmd := remoteRestartCmd(cmd, service, extra)
		if strings.TrimSpace(restartOverride) != "" {
			remoteCmd = remoteEnvPrefix(service, extra) + " && " + restartOverride
		}
		return remote.Run(target, remoteCmd, timeoutSec)
	}
	res := runShell(cmd, runtimeDir, serviceCmdEnv(service, runtimeDir, extra), timeoutSec)
	return res.Code, res.Output
}

func clipCmdOut(out string) string {
	if len(out) > 2000 {
		return out[len(out)-2000:]
	}
	return out
}

func shownServiceCmd(cmd, override string, service ServiceContract, runtimeDir, hash, machine string) string {
	extra := map[string]string{"APP_VERSION": hash, "DEPLOY_MACHINE": machine, "RUNTIME_DIR": runtimeDir}
	if strings.TrimSpace(override) != "" {
		return remoteEnvPrefix(service, extra) + " && " + override
	}
	return remoteRestartCmd(cmd, service, extra)
}

// runDeployRestart 是部署时的停再起：先 stopCmd，再 startCmd。
//
// 判定「有没有进程可停」是平台的事：看契约里的 healthUrl，不解析各服务 stop.sh 的文案。
// 所以业务方不用改 stop 脚本。stop 在部署里永远是 best-effort：失败只记 warning，
// 然后照常 start（没在跑、异常挂掉、stop 自己退出非 0 都一样）。start 才决定这次能不能成功。
func runDeployRestart(store *Store, job DeployJob, cfg Config, service ServiceContract,
	target MachineTarget, remote RemoteRunner, runtimeDir, hash, machine string) string {
	startCmd := strings.TrimSpace(service.StartCmd)
	stopCmd := strings.TrimSpace(service.StopCmd)
	restartCmd := strings.TrimSpace(service.RestartCmd)
	if restartCmd == "" {
		restartCmd = fmt.Sprintf("bash %q", filepath.Join(runtimeDir, "scripts", "restart.sh"))
	}
	override := ""
	if target.Remote() {
		override = target.ExpandRestartCmd(job.ServiceID, hash, runtimeDir, runtimeDir, machine)
	}
	run := func(cmd, ov string) (int, string) {
		return runServiceCmd(remote, target, cmd, service, runtimeDir, hash, machine, cfg.DeployMaxSec, ov)
	}
	logRemote := func(cmd, ov string) {
		if !target.Remote() {
			return
		}
		_ = store.AddDeployEvent(job.RequestID, eventlevel.Info,
			"远端执行（"+target.SSHDest()+"）："+shownServiceCmd(cmd, ov, service, runtimeDir, hash, machine))
	}
	up := serviceIsUp(target, remote, service)
	if !up {
		_ = store.AddDeployEvent(job.RequestID, eventlevel.Warn,
			"服务当前未在运行（健康检查未通过）：stop 按 best-effort，失败不阻断部署")
	}

	if override != "" {
		_ = store.AddDeployEvent(job.RequestID, eventlevel.Info,
			"使用该机器的 restart= 覆盖契约 restartCmd："+override)
		logRemote(restartCmd, override)
		code, out := run(restartCmd, override)
		if code != 0 && startCmd != "" {
			_ = store.AddDeployEvent(job.RequestID, eventlevel.Warn,
				"restart= 未成功（已忽略，改跑 startCmd）："+clipCmdOut(out))
			logRemote(startCmd, "")
			code, out = run(startCmd, "")
		}
		if code != 0 {
			_ = store.AddDeployEvent(job.RequestID, eventlevel.Error, "restart 失败："+clipCmdOut(out))
			return "restart failed: " + clipCmdOut(out)
		}
		return ""
	}

	if stopCmd != "" {
		fmt.Printf("[deploy] %s stop via contract: %s\n", job.RequestID, stopCmd)
		_ = store.AddDeployEvent(job.RequestID, eventlevel.Info, "执行 stopCmd："+stopCmd)
		logRemote(stopCmd, "")
		code, out := run(stopCmd, "")
		if code != 0 {
			_ = store.AddDeployEvent(job.RequestID, eventlevel.Warn,
				"stop 未成功，已忽略并继续 start："+clipCmdOut(out))
			fmt.Printf("[deploy] %s stop failed (ignored): %s\n", job.RequestID, clipCmdOut(out))
		} else {
			_ = store.AddDeployEvent(job.RequestID, eventlevel.Success, "stop 完成")
		}
	}

	if startCmd != "" {
		fmt.Printf("[deploy] %s start via contract (SERVICE_PORT=%s): %s\n",
			job.RequestID, servicePort(service), startCmd)
		_ = store.AddDeployEvent(job.RequestID, eventlevel.Info,
			"执行 startCmd（SERVICE_PORT="+servicePort(service)+"，同时注入 PORT）："+startCmd)
		logRemote(startCmd, "")
		code, out := run(startCmd, "")
		if code != 0 {
			_ = store.AddDeployEvent(job.RequestID, eventlevel.Error, "start 失败："+clipCmdOut(out))
			return "start failed: " + clipCmdOut(out)
		}
		return ""
	}

	fmt.Printf("[deploy] %s restart via contract (SERVICE_PORT=%s): %s\n",
		job.RequestID, servicePort(service), restartCmd)
	_ = store.AddDeployEvent(job.RequestID, eventlevel.Info,
		"执行 restartCmd（SERVICE_PORT="+servicePort(service)+"，同时注入 PORT）："+restartCmd)
	logRemote(restartCmd, "")
	code, out := run(restartCmd, "")
	if code != 0 {
		_ = store.AddDeployEvent(job.RequestID, eventlevel.Error, "restart 失败："+clipCmdOut(out))
		return "restart failed: " + clipCmdOut(out)
	}
	return ""
}

func runShell(cmdStr, cwd string, env []string, timeoutSec int) shellResult {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/bash", "-lc", cmdStr)
	cmd.Dir = cwd
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	if err := cmd.Start(); err != nil {
		return shellResult{Code: 1, Output: err.Error()}
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-ctx.Done():
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		<-done
		out := buf.String()
		if len(out) > 200_000 {
			out = out[len(out)-200_000:]
		}
		return shellResult{Code: 1, Output: out + "\ntimeout"}
	case err := <-done:
		out := buf.String()
		if len(out) > 200_000 {
			out = out[len(out)-200_000:]
		}
		code := 0
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else {
				code = 1
				out += "\n" + err.Error()
			}
		}
		return shellResult{Code: code, Output: out}
	}
}

func serviceIsUp(target MachineTarget, remote RemoteRunner, service ServiceContract) bool {
	url := serviceHealthURL(service)
	if strings.TrimSpace(url) == "" {
		return false
	}
	if target.Remote() {
		code, _ := remote.HTTP(target, "GET", url, "", 3)
		return code >= 200 && code < 300
	}
	return healthOK(url)
}

func healthOK(rawURL string) bool {
	client := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Cache-Control", "no-store")
	res, err := client.Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	return res.StatusCode >= 200 && res.StatusCode < 300
}

// waitForHealth polls the health endpoint every 2s until it returns healthy
// or timeout elapses. Use after running restartCmd: the restart command
// returning does not mean the service has rebound its port (node/webpack
// apps can take a few seconds), so a single check races the startup and
// falsely fails the deploy.
func waitForHealth(rawURL string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if healthOK(rawURL) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Second)
	}
}

func portFromHealthURL(healthURL string) string {
	u, err := url.Parse(healthURL)
	if err != nil {
		return "4211"
	}
	if u.Port() != "" {
		return u.Port()
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

// servicePort 返回给 start/stop/restart 脚本用的服务端口（同时注入 PORT 与
// SERVICE_PORT）。部署契约里显式声明的 port 是**必填项**；解析不到（老契约、
// 还没补填）时才退回 healthUrl 推导，保证这类服务仍然能部署。
func servicePort(service ServiceContract) string {
	if p := normalizePort(service.Port); p > 0 {
		return strconv.Itoa(p)
	}
	return portFromHealthURL(service.HealthURL)
}

func serviceCmdEnv(service ServiceContract, runtimeDir string, extra map[string]string) []string {
	base := os.Environ()
	envMap := make(map[string]string, len(base)+8)
	for _, e := range base {
		if i := strings.IndexByte(e, '='); i > 0 {
			envMap[e[:i]] = e[i+1:]
		}
	}
	for k, v := range extra {
		envMap[k] = v
	}
	port := servicePort(service)
	// RUNTIME_DIR 是**这次部署**用的目录（按目标机器平台选的，不一定是契约默认值）。
	if strings.TrimSpace(runtimeDir) == "" {
		runtimeDir = service.LocalRuntimeDir()
	}
	envMap["RUNTIME_DIR"] = runtimeDir
	envMap["SERVICE_PORT"] = port // 约定的正式字段名
	envMap["PORT"] = port         // 兼容：老脚本读 PORT
	delete(envMap, "HOST")
	delete(envMap, "DEPLOYMENT_HOME")

	out := make([]string, 0, len(envMap))
	for k, v := range envMap {
		out = append(out, k+"="+v)
	}
	return out
}

func externalWatchdogPausePath(runtimeDir string) string {
	name := filepath.Base(strings.TrimRight(runtimeDir, "/"))
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "deployment", name, "ops", "watchdog-pause-until")
}

func setExternalWatchdogPause(runtimeDir string, sec int) {
	file := externalWatchdogPausePath(runtimeDir)
	_ = os.MkdirAll(filepath.Dir(file), 0o755)
	if sec < 15 {
		sec = 15
	}
	until := time.Now().Unix() + int64(sec)
	if err := os.WriteFile(file, []byte(fmt.Sprintf("%d\n", until)), 0o644); err != nil {
		fmt.Printf("[deploy] watchdog-pause-until write failed: %v\n", err)
	}
}

func clearExternalWatchdogPause(runtimeDir string) {
	_ = os.Remove(externalWatchdogPausePath(runtimeDir))
}

func clearStaleDeployPauses(store *Store) int {
	svcs, err := store.ListServices()
	if err != nil {
		return 0
	}
	n := 0
	for _, svc := range svcs {
		file := externalWatchdogPausePath(svc.RuntimeDir)
		if _, err := os.Stat(file); err != nil {
			continue
		}
		clearExternalWatchdogPause(svc.RuntimeDir)
		_ = os.Remove(filepath.Join(svc.RuntimeDir, "backend", ".watchdog-paused"))
		fmt.Printf("[deploy] cleared stale pause flags for %s\n", svc.ServiceID)
		n++
	}
	return n
}

func executeDeploy(store *Store, cfg Config, storage ArtifactStorage, drain *GracefulDrain,
	machines *MachineCatalog, remote RemoteRunner, requestID string) {
	job, err := store.GetDeploy(requestID)
	if err != nil || job == nil || job.State != StateRunning {
		return
	}
	// On exit, if this is the draining self-deploy that did NOT hand off to
	// the upgrader (i.e. failed before handoff), release the drain so workers
	// can resume. On a successful handoff the process is killed, so we keep
	// draining to prevent new work starting right before the kill.
	handedOff := false
	defer func() {
		if !handedOff && drain != nil && drain.IsDraining() && drain.RestartingID() == requestID {
			drain.Clear()
			fmt.Printf("[graceful] drain cleared (deploy %s did not hand off)\n", requestID)
		}
	}()
	service, err := store.GetService(job.ServiceID)
	if err != nil || service == nil {
		_, _ = store.FinishDeploy(job.RequestID, FinishPatch{
			State: StateFailed,
			Error: fmt.Sprintf("unknown service: %s", job.ServiceID),
		})
		return
	}

	tag, err := normalizeDeploymentTag(job.Deployment)
	if err != nil {
		_, _ = store.FinishDeploy(job.RequestID, FinishPatch{State: StateFailed, Error: err.Error()})
		return
	}
	hash := deploymentHash(tag)
	// 本次部署落到的「部署机器」：触发时选择并随任务转发而来；空/未知 → 默认机器。
	// 已知机器来自注册中心（+ 本机 + DEPLOY_MACHINES），见 MachineCatalog。
	machine, mErr := machines.ValidateForService(context.Background(), job.ServiceID, job.TargetMachine)
	if mErr != nil {
		machine = machines.DefaultFor(context.Background(), job.ServiceID)
		_ = store.AddDeployEvent(job.RequestID, eventlevel.Warn,
			"部署机器不可用（"+mErr.Error()+"），退回默认机器="+machine)
	}
	target, hasTarget := machines.TargetForService(context.Background(), job.ServiceID, machine)
	if !hasTarget {
		// 校验层已经保证有通道；这里是兜底（例如通道配置在任务排队期间被改掉）。
		_, _ = store.FinishDeploy(job.RequestID, FinishPatch{
			State: StateFailed,
			Error: "deploy machine " + machine + " has no deploy channel (" + deployMachineTargetsEnv + ")",
		})
		return
	}
	// runtime 目录按**目标机器**的平台选：本机 = 控制面自己的 GOOS；远端 = 预检 probe 的
	// uname。契约里 runtimeDirs{darwin,linux} 优先，没配这个平台就回落默认 runtimeDir；
	// 远端再兜通道约定的 <remote-runtime-home>/<serviceId>。
	remotePlatform := ""
	runtimeDir := ""
	if target.Remote() {
		probe, perr := remoteProbe(store, *job, target, remote)
		if perr != nil {
			_, _ = store.FinishDeploy(job.RequestID, FinishPatch{State: StateFailed, Error: perr.Error()})
			return
		}
		remotePlatform = probe.platform
		runtimeDir = service.RuntimeDirForRemote(probe.OS, job.ServiceID, target)
		if runtimeDir == "" {
			_, _ = store.FinishDeploy(job.RequestID, FinishPatch{
				State: StateFailed,
				Error: "no runtime dir for machine " + machine +
					": set runtimeDir/runtimeDirs on the contract or a remote runtime home in the deploy channel",
			})
			return
		}
	} else {
		runtimeDir = service.LocalRuntimeDir()
		if runtimeDir == "" {
			_, _ = store.FinishDeploy(job.RequestID, FinishPatch{
				State: StateFailed,
				Error: "service contract has no runtimeDir",
			})
			return
		}
	}
	// 产物平台 vs 机器平台：tag 上带着产物平台，机器平台已知（本机 GOOS / 远端 uname），
	// 不一致就**不下载、不推送**，直接说清怎么办。
	if err := assertTagPlatformMatchesMachine(tag, machine, target, remotePlatform); err != nil {
		_ = store.AddDeployEvent(job.RequestID, eventlevel.Error, err.Error())
		_, _ = store.FinishDeploy(job.RequestID, FinishPatch{State: StateFailed, Error: err.Error()})
		return
	}
	startMsg := "开始部署：service=" + job.ServiceID + " deployment=" + tag + " version=" + hash +
		" 部署机器=" + machine
	if by := job.Identity().String(); by != "" {
		startMsg += " 触发者=" + by
	}
	_ = store.AddDeployEvent(job.RequestID, eventlevel.Info, startMsg)
	if target.Remote() {
		_ = store.AddDeployEvent(job.RequestID, eventlevel.Info,
			"远端部署："+target.Describe()+" → runtime="+runtimeDir+"（平台 "+remotePlatform+"）")
		// 目录预检：不像这个服务的 runtime 就拒绝 rsync --delete；不存在则建（首次部署）。
		if code, out := remoteDirCheck(store, *job, target, remote, runtimeDir); code != 0 {
			_, _ = store.FinishDeploy(job.RequestID, FinishPatch{State: StateFailed, Error: out})
			return
		}
	}
	// Fetch the package from the configured storage backend into a temp dir;
	// the storage is the single source of truth for the bytes (local disk or
	// GitHub Releases, etc.). Prefer the access path stored in the local
	// artifacts table (avoids a resolution round-trip); fall back to resolving
	// via the backend.
	src, err := os.MkdirTemp("", tempPrefixDeployPkg+"*")
	if err != nil {
		_, _ = store.FinishDeploy(job.RequestID, FinishPatch{State: StateFailed, Error: err.Error()})
		return
	}
	// 自升级（self-deploy）时本进程会被 acp-upgrader 杀掉，这个 defer 不会执行 ——
	// 那种情况由下次启动时的残留清理兜底（cleanupStaleTempWork）。
	defer func() {
		if err := removeAllForce(src); err != nil {
			fmt.Printf("[deploy] %s warn: 清理临时目录 %s 失败: %v\n", job.RequestID, src, err)
		}
	}()
	var accessPath string
	if art, _ := store.GetArtifact(job.ServiceID, tag); art != nil {
		accessPath = art.AssetURL
	}
	dlCtx, dlCancel := context.WithTimeout(context.Background(), time.Duration(cfg.ReleaseMaxSec)*time.Second)
	defer dlCancel()
	_ = store.AddDeployEvent(job.RequestID, eventlevel.Info,
		"下载开始：storage="+storage.Name()+" tag="+tag)
	dlStart := time.Now()
	if err := storage.Download(dlCtx, job.ServiceID, service.GitRepoURL, tag, src, accessPath); err != nil {
		_ = store.AddDeployEvent(job.RequestID, eventlevel.Error, "下载失败："+err.Error())
		_, _ = store.FinishDeploy(job.RequestID, FinishPatch{
			State: StateFailed,
			Error: fmt.Sprintf("download package: %v", err),
		})
		return
	}
	_ = store.AddDeployEvent(job.RequestID, eventlevel.Success,
		"下载结束：storage="+storage.Name()+" tag="+tag+
			" size="+humanBytes(dirSize(src))+" 耗时="+humanDuration(time.Since(dlStart)))

	if target.Remote() {
		// 产物平台检查：本机打的包（例如 macOS Mach-O）推到 Linux 远端是起不来的，
		// 必须在这里拦住 —— 推送之后远端服务已经停了，损失更大。
		if err := remotePlatformGuard(store, *job, src, remotePlatform); err != nil {
			_, _ = store.FinishDeploy(job.RequestID, FinishPatch{State: StateFailed, Error: err.Error()})
			return
		}
	}

	self := isSelfDeploy(*service, cfg)
	if self {
		bin := filepath.Join(src, "bin", "deployment-server")
		st, err := os.Stat(bin)
		if err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
			_, _ = store.FinishDeploy(job.RequestID, FinishPatch{
				State: StateFailed,
				Error: fmt.Sprintf("self-upgrade package missing executable bin/deployment-server: %s", bin),
			})
			return
		}
	} else if _, err := os.Stat(filepath.Join(src, "scripts", "restart.sh")); err != nil && strings.TrimSpace(service.RestartCmd) == "" {
		_, _ = store.FinishDeploy(job.RequestID, FinishPatch{
			State: StateFailed,
			Error: fmt.Sprintf("package incomplete and no restartCmd: %s", src),
		})
		return
	}
	snapVerBytes, err := os.ReadFile(filepath.Join(src, "VERSION"))
	if err != nil {
		_, _ = store.FinishDeploy(job.RequestID, FinishPatch{State: StateFailed, Error: err.Error()})
		return
	}
	snapVer := strings.TrimSpace(string(snapVerBytes))
	if snapVer != hash {
		_, _ = store.FinishDeploy(job.RequestID, FinishPatch{
			State: StateFailed,
			Error: fmt.Sprintf("VERSION(%s) != hash(%s)", snapVer, hash),
		})
		return
	}

	// 本机部署才需要本地 runtime 目录；远端部署落到远端目录（runtimeDir 指向那边）。
	if !target.Remote() {
		_ = os.MkdirAll(runtimeDir, 0o755)
	}

	if service.SupportsGracefulRestart() {
		fmt.Printf("[deploy] %s graceful restart enabled (notify+poll)\n", job.RequestID)
		var gracefulTransport gracefulTransport = localTransport{}
		if target.Remote() {
			gracefulTransport = remoteTransport{target: target, runner: remote}
			_ = store.AddDeployEvent(job.RequestID, eventlevel.Info,
				"graceful：在远端 "+target.SSHHost+" 上发通知/轮询（用远端自己的 127.0.0.1 地址）")
		}
		if waitForGracefulRestartVia(store, *service, cfg, *job, hash, gracefulTransport) {
			fmt.Printf("[deploy] %s proceeding after graceful force timeout\n", job.RequestID)
		}
	} else {
		fmt.Printf("[deploy] %s no graceful endpoints in registry; direct restart\n", job.RequestID)
	}

	var rsyncCmd string
	if self {
		rsyncCmd = selfDeployRsyncCmd(src, runtimeDir)
	} else {
		rsyncCmd = strings.Join([]string{
			"rsync", "-a", "--delete",
			"--filter='P backend/.env'",
			"--filter='P backend/data/'",
			"--filter='P backend/runtime.pid'",
			"--filter='P backend/server.log'",
			"--filter='P backend/.watchdog-paused'",
			"--exclude='backend/.env'",
			"--exclude='backend/data/'",
			"--exclude='backend/runtime.pid'",
			"--exclude='backend/server.log'",
			"--exclude='backend/.watchdog-paused'",
			"--exclude='.git/'",
			fmt.Sprintf("%q", src+"/"),
			fmt.Sprintf("%q", runtimeDir+"/"),
		}, " ")
	}

	if target.Remote() {
		fmt.Printf("[deploy] %s rsync %s → %s:%s\n", job.RequestID, tag, target.SSHDest(), runtimeDir)
		code, out := remote.PushDir(target, src, runtimeDir, true, cfg.DeployMaxSec)
		if code != 0 {
			if len(out) > 2000 {
				out = out[len(out)-2000:]
			}
			_ = store.AddDeployEvent(job.RequestID, eventlevel.Error, "远端 rsync 失败："+out)
			_, _ = store.FinishDeploy(job.RequestID, FinishPatch{
				State: StateFailed,
				Error: "remote rsync failed: " + out,
			})
			return
		}
		_ = store.AddDeployEvent(job.RequestID, eventlevel.Success,
			"制品已 rsync 到远端 runtime："+target.SSHDest()+":"+runtimeDir)
		stamp := "printf '%s\n' " + shellQuote(hash) + " > " + shellQuote(runtimeDir+"/VERSION") +
			" && printf '%s\n' " + shellQuote(tag) + " > " + shellQuote(runtimeDir+"/DEPLOYMENT")
		if code, out := remote.Run(target, stamp, 30); code != 0 {
			_ = store.AddDeployEvent(job.RequestID, eventlevel.Error, "远端写入 VERSION/DEPLOYMENT 失败："+out)
			_, _ = store.FinishDeploy(job.RequestID, FinishPatch{
				State: StateFailed,
				Error: "remote stamp failed: " + out,
			})
			return
		}
	} else {
		fmt.Printf("[deploy] %s rsync %s → %s\n", job.RequestID, tag, runtimeDir)
		rsync := runShell(rsyncCmd, cfg.Home, os.Environ(), cfg.DeployMaxSec)
		if rsync.Code != 0 {
			out := rsync.Output
			if len(out) > 2000 {
				out = out[len(out)-2000:]
			}
			_ = store.AddDeployEvent(job.RequestID, eventlevel.Error, "rsync 失败："+out)
			_, _ = store.FinishDeploy(job.RequestID, FinishPatch{
				State: StateFailed,
				Error: "rsync failed: " + out,
			})
			return
		}
		_ = store.AddDeployEvent(job.RequestID, eventlevel.Success,
			"制品已 rsync 到 runtime："+runtimeDir)

		_ = os.WriteFile(filepath.Join(runtimeDir, "VERSION"), []byte(hash+"\n"), 0o644)
		_ = os.WriteFile(filepath.Join(runtimeDir, "DEPLOYMENT"), []byte(tag+"\n"), 0o644)
	}

	if self {
		if !upgraderRunning(cfg.Home) {
			_ = store.AddDeployEvent(job.RequestID, eventlevel.Error,
				"acp-upgrader 未运行；请启动 scripts/upgrader-start.sh")
			_, _ = store.FinishDeploy(job.RequestID, FinishPatch{
				State:   StateFailed,
				Version: hash,
				Error:   "artifacts staged but acp-upgrader is not running; start scripts/upgrader-start.sh",
			})
			return
		}
		if err := enqueueACPUpgrade(cfg, *job, *service, hash, tag); err != nil {
			_ = store.AddDeployEvent(job.RequestID, eventlevel.Error, "写入 upgrade-requests 失败："+err.Error())
			_, _ = store.FinishDeploy(job.RequestID, FinishPatch{
				State:   StateFailed,
				Version: hash,
				Error:   "failed to enqueue acp-upgrader request: " + err.Error(),
			})
			return
		}
		fmt.Printf("[deploy] %s self-upgrade staged; handed off to acp-upgrader (job stays running until reconcile)\n", job.RequestID)
		_ = store.AddDeployEvent(job.RequestID, eventlevel.Info,
			"已移交 acp-upgrader：停止旧服务 → 启动新服务 → 探活（任务保持 running 直到新进程 reconcile）")
		handedOff = true
		return
	}

	// 新机器第一次部署：制品 rsync 不传 backend/.env，这里从 example 补一份，
	// 避免 start.sh 因「缺少 .env」直接失败。已有文件不会被覆盖。
	seedRuntimeEnvIfMissing(store, *job, target, remote, runtimeDir)

	// 服务端口是必填项；老契约（没说）先按 healthUrl 推导，但在时间线上明确标出来。
	if normalizePort(service.Port) == 0 {
		_ = store.AddDeployEvent(job.RequestID, eventlevel.Warn,
			"部署契约未指定服务端口（port）：本次按 healthUrl 推导 SERVICE_PORT="+servicePort(*service)+
				"，请在「服务契约」里补填")
	}
	pauseSec := cfg.DeployMaxSec
	if pauseSec > 90 {
		pauseSec = 90
	}
	if target.Remote() {
		// 外部 watchdog 的暂停标记是本机文件的机制；远端部署不适用（远端服务自己的
		// 看门狗按它自己的规矩来）。
		_ = store.AddDeployEvent(job.RequestID, eventlevel.Info,
			"远端部署：跳过本机 watchdog 暂停标记（本机机制）")
	} else {
		setExternalWatchdogPause(runtimeDir, pauseSec)
		defer clearExternalWatchdogPause(runtimeDir)
	}

	if failMsg := runDeployRestart(store, *job, cfg, *service, target, remote, runtimeDir, hash, machine); failMsg != "" {
		_, _ = store.FinishDeploy(job.RequestID, FinishPatch{
			State:   StateFailed,
			Version: hash,
			Error:   failMsg,
		})
		return
	}

	// 契约里只存路径（/health），这里按端口拼出真地址：本机直接请求，远端在那台机器上 curl。
	healthURL := serviceHealthURL(*service)
	healthOK := waitForHealth(healthURL, cfg.HealthCheckTimeout)
	if target.Remote() {
		healthOK = waitForRemoteHealth(remote, target, healthURL, cfg.HealthCheckTimeout)
	}
	if !healthOK {
		if target.Remote() {
			_ = store.AddDeployEvent(job.RequestID, eventlevel.Error,
				"远端健康检查失败："+target.SSHHost+" 上的 "+healthURL)
		} else {
			_ = store.AddDeployEvent(job.RequestID, eventlevel.Error, "健康检查失败："+healthURL)
		}
		_, _ = store.FinishDeploy(job.RequestID, FinishPatch{
			State:   StateFailed,
			Version: hash,
			Error:   "restart finished but health check failed: " + healthURL,
		})
		return
	}
	_ = store.AddDeployEvent(job.RequestID, eventlevel.Success,
		"健康检查通过："+healthURL)

	_, _ = store.FinishDeploy(job.RequestID, FinishPatch{
		State:   StateSucceeded,
		Version: hash,
		Message: "deploy succeeded",
	})
	successMsg := "部署成功：version=" + hash
	if target.Remote() {
		successMsg += "（远端 " + machine + "：" + target.SSHDest() + ":" + runtimeDir + "）"
	}
	_ = store.AddDeployEvent(job.RequestID, eventlevel.Success, successMsg)
	fmt.Printf("[deploy] %s ok version=%s\n", job.RequestID, hash)
}

func reconcileOrphanDeploys(store *Store) int {
	orphans, err := store.ListDeploysByState(StateRunning)
	if err != nil {
		return 0
	}
	n := 0
	for _, job := range orphans {
		service, err := store.GetService(job.ServiceID)
		if err != nil || service == nil {
			_, _ = store.FinishDeploy(job.RequestID, FinishPatch{
				State:   StateFailed,
				Error:   "deployment service restarted; service contract missing",
				Message: "reconciled after deployment service restart",
			})
			n++
			continue
		}
		tag, _ := normalizeDeploymentTag(job.Deployment)
		hash := deploymentHash(tag)
		versionOnDisk := ""
		if b, err := os.ReadFile(filepath.Join(service.LocalRuntimeDir(), "VERSION")); err == nil {
			versionOnDisk = strings.TrimSpace(string(b))
		}
		// 契约里只存路径（/health），探活要用按本机端口拼出来的真地址 —— 否则
		// 自部署收尾时把 "/health" 当 URL 请求，部署会被误判为失败。
		ok := healthOK(serviceHealthURL(*service))
		if ok && versionOnDisk == hash {
			_, _ = store.FinishDeploy(job.RequestID, FinishPatch{
				State:   StateSucceeded,
				Version: hash,
				Message: "deploy succeeded (status reconciled after deployment service died mid-restart; gateway health confirmed)",
			})
			fmt.Printf("[deploy] reconciled %s → succeeded (health ok, version=%s)\n", job.RequestID, hash)
		} else {
			errMsg := fmt.Sprintf("deployment service restarted mid-deploy; health check failed: %s", serviceHealthURL(*service))
			if ok {
				errMsg = fmt.Sprintf("deployment service restarted mid-deploy; VERSION=%s expected=%s", versionOnDisk, hash)
			}
			_, _ = store.FinishDeploy(job.RequestID, FinishPatch{
				State:   StateFailed,
				Version: versionOnDisk,
				Error:   errMsg,
				Message: "reconciled after deployment service restart",
			})
			fmt.Printf("[deploy] reconciled %s → failed\n", job.RequestID)
		}
		n++
	}
	return n
}

type DeployWorker struct {
	store    *Store
	cfg      Config
	storage  ArtifactStorage
	drain    *GracefulDrain
	machines *MachineCatalog
	remote   RemoteRunner
	mu       sync.Mutex
	ticking  bool
	// active maps a deployment unit (a runtime dir) to the requestID currently
	// deploying it. Deploys of *different* units run concurrently; the same
	// unit never runs twice at once (two contracts can share one runtimeDir,
	// e.g. the seeded `web-cursor` and the registry `agent-control-plane`).
	active   map[string]string
	stopCh   chan struct{}
	loopWg   sync.WaitGroup
	deployWg sync.WaitGroup
	// run executes one claimed deploy. Overridable in tests.
	run func(requestID string)
}

func NewDeployWorker(store *Store, cfg Config, storage ArtifactStorage, drain *GracefulDrain,
	machines *MachineCatalog, remote RemoteRunner) *DeployWorker {
	if machines == nil {
		// 没给机器目录（测试/未注入）：按本地配置兜一个，本机永远是已知机器。
		machines = NewMachineCatalog(cfg, nil)
	}
	if remote == nil {
		remote = sshRemoteRunner{home: cfg.Home}
	}
	return &DeployWorker{
		store:    store,
		cfg:      cfg,
		storage:  storage,
		drain:    drain,
		machines: machines,
		remote:   remote,
		active:   map[string]string{},
		stopCh:   make(chan struct{}),
		run: func(requestID string) {
			executeDeploy(store, cfg, storage, drain, machines, remote, requestID)
		},
	}
}

func (w *DeployWorker) Start() {
	w.loopWg.Add(1)
	go func() {
		defer w.loopWg.Done()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		w.tick()
		for {
			select {
			case <-w.stopCh:
				return
			case <-t.C:
				w.tick()
			}
		}
	}()
}

func (w *DeployWorker) Stop() {
	select {
	case <-w.stopCh:
	default:
		close(w.stopCh)
	}
	w.loopWg.Wait()   // no new claims
	w.deployWg.Wait() // let the in-flight deploys finish
}

func (w *DeployWorker) Kick() {
	go w.tick()
}

// tick claims every queued deploy whose runtime is idle and runs each one in
// its own goroutine.
//
// It used to run exactly one deploy at a time (single `busy` flag + a
// synchronous executeDeploy), which meant one service sitting in its peer's
// graceful restart window blocked the whole queue: the peer is polled for up to
// `gracefulRestartMaxWaitMs` (default 10 minutes) before the deploy is forced,
// and during that time every later deploy — including deploys of unrelated
// services — stayed `queued` while its pipeline showed `deploying`. Deploys of
// different services touch different runtimeDir/processes, so they are run
// concurrently; only the same runtimeDir is serialized.
func (w *DeployWorker) tick() {
	if w.drain.IsDraining() {
		// graceful self-restart in progress: do not claim new deploys
		return
	}
	w.mu.Lock()
	if w.ticking {
		w.mu.Unlock()
		return
	}
	w.ticking = true
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.ticking = false
		w.mu.Unlock()
	}()

	queued, err := w.store.QueuedDeploys()
	if err != nil {
		fmt.Printf("[deploy-worker] %v\n", err)
		return
	}
	for _, candidate := range queued {
		if w.drain.IsDraining() {
			return
		}
		unit, ok := w.reserveUnit(candidate.ServiceID, candidate.RequestID)
		if !ok {
			// another deploy of the same runtime is in flight; its turn comes
			// when that one finishes (the next tick picks it up)
			continue
		}
		job, err := w.store.ClaimQueued(candidate.RequestID)
		if err != nil {
			w.releaseUnit(unit, candidate.RequestID)
			fmt.Printf("[deploy-worker] claim %s: %v\n", candidate.RequestID, err)
			continue
		}
		if job == nil {
			// lost the race (another tick/process claimed it)
			w.releaseUnit(unit, candidate.RequestID)
			continue
		}
		w.deployWg.Add(1)
		go func(job DeployJob, unit string) {
			defer w.deployWg.Done()
			defer w.releaseUnit(unit, job.RequestID)
			w.run(job.RequestID)
		}(*job, unit)
	}
}

// deployUnit is the mutual-exclusion key for a deploy: the service's runtime
// dir, so two contracts pointing at the same runtime never deploy at once.
func (w *DeployWorker) deployUnit(serviceID string) string {
	if svc, err := w.store.GetService(serviceID); err == nil && svc != nil {
		if dir := svc.LocalRuntimeDir(); dir != "" {
			return filepath.Clean(dir)
		}
	}
	return "service:" + serviceID
}

func (w *DeployWorker) reserveUnit(serviceID, requestID string) (string, bool) {
	unit := w.deployUnit(serviceID)
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, busy := w.active[unit]; busy {
		return unit, false
	}
	w.active[unit] = requestID
	return unit, true
}

// releaseUnit only drops the reservation this requestID holds, so a tick that
// reserved the same unit and then lost the claim race can never free a unit
// that the winner is using.
func (w *DeployWorker) releaseUnit(unit, requestID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.active[unit] == requestID {
		delete(w.active, unit)
	}
}
