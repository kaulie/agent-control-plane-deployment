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

func serviceCmdEnv(service ServiceContract, extra map[string]string) []string {
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
	envMap["RUNTIME_DIR"] = service.RuntimeDir
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

func executeDeploy(store *Store, cfg Config, storage ArtifactStorage, drain *GracefulDrain, requestID string) {
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
	hash := strings.TrimPrefix(tag, "deployment-")
	startMsg := "开始部署：service=" + job.ServiceID + " deployment=" + tag + " version=" + hash
	if by := job.Identity().String(); by != "" {
		startMsg += " 触发者=" + by
	}
	_ = store.AddDeployEvent(job.RequestID, eventlevel.Info, startMsg)
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

	_ = os.MkdirAll(service.RuntimeDir, 0o755)

	if service.SupportsGracefulRestart() {
		fmt.Printf("[deploy] %s graceful restart enabled (notify+poll)\n", job.RequestID)
		if waitForGracefulRestart(store, *service, cfg, *job, hash) {
			fmt.Printf("[deploy] %s proceeding after graceful force timeout\n", job.RequestID)
		}
	} else {
		fmt.Printf("[deploy] %s no graceful endpoints in registry; direct restart\n", job.RequestID)
	}

	var rsyncCmd string
	if self {
		rsyncCmd = selfDeployRsyncCmd(src, service.RuntimeDir)
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
			fmt.Sprintf("%q", service.RuntimeDir+"/"),
		}, " ")
	}

	fmt.Printf("[deploy] %s rsync %s → %s\n", job.RequestID, tag, service.RuntimeDir)
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
		"制品已 rsync 到 runtime："+service.RuntimeDir)

	_ = os.WriteFile(filepath.Join(service.RuntimeDir, "VERSION"), []byte(hash+"\n"), 0o644)
	_ = os.WriteFile(filepath.Join(service.RuntimeDir, "DEPLOYMENT"), []byte(tag+"\n"), 0o644)

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

	restartCmd := strings.TrimSpace(service.RestartCmd)
	if restartCmd == "" {
		restartCmd = fmt.Sprintf("bash %q", filepath.Join(service.RuntimeDir, "scripts", "restart.sh"))
	}
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
	setExternalWatchdogPause(service.RuntimeDir, pauseSec)
	defer clearExternalWatchdogPause(service.RuntimeDir)

	fmt.Printf("[deploy] %s restart via contract (SERVICE_PORT=%s): %s\n",
		job.RequestID, servicePort(*service), restartCmd)
	_ = store.AddDeployEvent(job.RequestID, eventlevel.Info,
		"执行 restartCmd（SERVICE_PORT="+servicePort(*service)+"，同时注入 PORT）："+restartCmd)
	restart := runShell(
		restartCmd,
		service.RuntimeDir,
		serviceCmdEnv(*service, map[string]string{"APP_VERSION": hash}),
		cfg.DeployMaxSec,
	)
	if restart.Code != 0 {
		out := restart.Output
		if len(out) > 2000 {
			out = out[len(out)-2000:]
		}
		_ = store.AddDeployEvent(job.RequestID, eventlevel.Error, "restart 失败："+out)
		_, _ = store.FinishDeploy(job.RequestID, FinishPatch{
			State:   StateFailed,
			Version: hash,
			Error:   "restart failed: " + out,
		})
		return
	}

	if !waitForHealth(service.HealthURL, cfg.HealthCheckTimeout) {
		_ = store.AddDeployEvent(job.RequestID, eventlevel.Error, "健康检查失败："+service.HealthURL)
		_, _ = store.FinishDeploy(job.RequestID, FinishPatch{
			State:   StateFailed,
			Version: hash,
			Error:   "restart finished but health check failed: " + service.HealthURL,
		})
		return
	}
	_ = store.AddDeployEvent(job.RequestID, eventlevel.Success,
		"健康检查通过："+service.HealthURL)

	_, _ = store.FinishDeploy(job.RequestID, FinishPatch{
		State:   StateSucceeded,
		Version: hash,
		Message: "deploy succeeded",
	})
	_ = store.AddDeployEvent(job.RequestID, eventlevel.Success, "部署成功：version="+hash)
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
		hash := strings.TrimPrefix(tag, "deployment-")
		versionOnDisk := ""
		if b, err := os.ReadFile(filepath.Join(service.RuntimeDir, "VERSION")); err == nil {
			versionOnDisk = strings.TrimSpace(string(b))
		}
		ok := healthOK(service.HealthURL)
		if ok && versionOnDisk == hash {
			_, _ = store.FinishDeploy(job.RequestID, FinishPatch{
				State:   StateSucceeded,
				Version: hash,
				Message: "deploy succeeded (status reconciled after deployment service died mid-restart; gateway health confirmed)",
			})
			fmt.Printf("[deploy] reconciled %s → succeeded (health ok, version=%s)\n", job.RequestID, hash)
		} else {
			errMsg := fmt.Sprintf("deployment service restarted mid-deploy; health check failed: %s", service.HealthURL)
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
	store   *Store
	cfg     Config
	storage ArtifactStorage
	drain   *GracefulDrain
	mu      sync.Mutex
	busy    bool
	stopCh  chan struct{}
	wg      sync.WaitGroup
}

func NewDeployWorker(store *Store, cfg Config, storage ArtifactStorage, drain *GracefulDrain) *DeployWorker {
	return &DeployWorker{
		store:   store,
		cfg:     cfg,
		storage: storage,
		drain:   drain,
		stopCh:  make(chan struct{}),
	}
}

func (w *DeployWorker) Start() {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
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
	w.wg.Wait()
}

func (w *DeployWorker) Kick() {
	go w.tick()
}

func (w *DeployWorker) tick() {
	w.mu.Lock()
	if w.busy {
		w.mu.Unlock()
		return
	}
	if w.drain.IsDraining() {
		// graceful self-restart in progress: do not claim new deploys
		w.mu.Unlock()
		return
	}
	w.busy = true
	w.mu.Unlock()

	defer func() {
		w.mu.Lock()
		w.busy = false
		w.mu.Unlock()
	}()

	job, err := w.store.ClaimNextQueued()
	if err != nil {
		fmt.Printf("[deploy-worker] %v\n", err)
		return
	}
	if job == nil {
		return
	}
	executeDeploy(w.store, w.cfg, w.storage, w.drain, job.RequestID)
}
