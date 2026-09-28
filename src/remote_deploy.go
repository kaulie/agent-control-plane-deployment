package main

import (
	"sort"
	"strings"
	"time"

	"github.com/kaulie/agent-control-plane-deployment/eventlevel"
)

// 远端部署的三块小工具：部署前预检、远端 restart 命令、远端健康检查。

// remotePreflight 在真正推制品之前确认「远端确实能干活」：ssh 能登、curl 在、目标
// runtime 目录长得像这个服务的 runtime（有 scripts/ —— 否则一次 rsync --delete 可能
// 删掉别的东西）。返回 code != 0 时 out 就是给人看的原因。
func remotePreflight(store *Store, job DeployJob, target MachineTarget, remote RemoteRunner,
	runtimeDir string) (int, string) {
	check := strings.Join([]string{
		"set -e",
		"command -v curl >/dev/null || { echo 'remote host has no curl (needed for graceful/health checks)'; exit 3; }",
		"if [ -d " + shellQuote(runtimeDir) + " ]; then",
		"  [ -f " + shellQuote(runtimeDir+"/scripts/restart.sh") + " ] || { echo " +
			shellQuote(runtimeDir+" exists but is not this service's runtime (no scripts/restart.sh); refusing to rsync --delete") + "; exit 4; }",
		"else",
		"  mkdir -p " + shellQuote(runtimeDir),
		"  echo 'runtime dir created (first deploy)'",
		"fi",
	}, "\n")
	_ = store.AddDeployEvent(job.RequestID, eventlevel.Info,
		"远端预检：ssh "+target.SSHDest()+"（curl / runtime 目录 "+runtimeDir+"）")
	code, out := remote.Run(target, check, 60)
	out = strings.TrimSpace(out)
	if code != 0 {
		if strings.Contains(out, "Permission denied") || strings.Contains(out, "publickey") {
			// 免密通常配在 ~/.ssh/config 的别名上（Host <alias> → HostName/User/IdentityFile）：
			// 用字面 host 登录时别名不生效，密钥就找不到。
			out += "\n提示：ssh 免密若配在 ~/.ssh/config 的别名上，请把通道写成 `ssh <别名> <remote-home>`" +
				"（例如 `" + target.ID + "=ssh agent-oversea /home/ubuntu/runtime`）"
		}
		_ = store.AddDeployEvent(job.RequestID, eventlevel.Error, "远端预检失败："+out)
		return code, "remote preflight failed: " + out
	}
	_ = store.AddDeployEvent(job.RequestID, eventlevel.Success, "远端预检通过："+out)
	return 0, ""
}

// remoteRestartCmd 把 restartCmd 包成一条在远端跑的 shell：cd 到 runtime，注入
// SERVICE_PORT / PORT / APP_VERSION / DEPLOY_MACHINE / RUNTIME_DIR，再跑契约里的命令。
func remoteRestartCmd(restartCmd string, service ServiceContract, extra map[string]string) string {
	env := map[string]string{}
	port := servicePort(service)
	env["SERVICE_PORT"] = port
	env["PORT"] = port
	for k, v := range extra {
		if strings.TrimSpace(v) != "" {
			env[k] = v
		}
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	assign := make([]string, 0, len(keys))
	for _, k := range keys {
		assign = append(assign, k+"="+shellQuote(env[k]))
	}
	// 本地那一路是 cwd=runtimeDir 跑 restartCmd；远端也一样先 cd 过去。
	runtimeDir := strings.TrimSpace(extra["RUNTIME_DIR"])
	cmd := "export " + strings.Join(assign, " ") + " && " + restartCmd
	if runtimeDir != "" {
		cmd = "cd " + shellQuote(runtimeDir) + " && " + cmd
	}
	return cmd
}

// waitForRemoteHealth 在远端用 curl 探活（healthUrl 是服务自己的 127.0.0.1 地址，
// 站在那台机器上问才对），语义与 waitForHealth 一致。
func waitForRemoteHealth(remote RemoteRunner, target MachineTarget, healthURL string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if code, _ := remote.HTTP(target, "GET", healthURL, "", 3); code >= 200 && code < 300 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Second)
	}
}
