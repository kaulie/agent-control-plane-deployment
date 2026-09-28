package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kaulie/agent-control-plane-deployment/eventlevel"
)

// 远端部署的三块小工具：部署前预检、远端 restart 命令、远端健康检查。

// remoteProbe 是远端预检的第一段：ssh 能不能登、远端有没有 curl、远端是什么平台。
// 放在最前面（下载之前）：不通就秒级失败，不白下载几百 MB，也不在 graceful 里空转；
// 平台还要用来选 runtime 目录（mac/linux 两套路径）与做产物平台检查。
type remoteProbeResult struct {
	OS       string // darwin / linux（uname -s 归一后）
	Arch     string
	platform string // "<os>/<arch>"，事件与产物检查用
}

func remoteProbe(store *Store, job DeployJob, target MachineTarget, remote RemoteRunner) (remoteProbeResult, error) {
	var out remoteProbeResult
	script := strings.Join([]string{
		"set -e",
		"command -v curl >/dev/null || { echo 'remote host has no curl (needed for graceful/health checks)'; exit 3; }",
		"uname -s",
		"uname -m",
	}, "\n")
	_ = store.AddDeployEvent(job.RequestID, eventlevel.Info,
		"远端预检：ssh "+target.SSHDest()+"（连通性 / curl / 平台）")
	code, raw := remote.Run(target, script, 60)
	outText := strings.TrimSpace(raw)
	if code != 0 {
		if strings.Contains(outText, "Permission denied") || strings.Contains(outText, "publickey") {
			// 免密通常配在 ~/.ssh/config 的别名上（Host <alias> → HostName/User/IdentityFile）：
			// 用字面 host 登录时别名不生效，密钥就找不到。
			outText += "\n提示：ssh 免密若配在 ~/.ssh/config 的别名上，请把通道写成 `ssh <别名> <remote-home>`" +
				"（例如 `" + target.ID + "=ssh agent-oversea /home/ubuntu/runtime`）"
		}
		_ = store.AddDeployEvent(job.RequestID, eventlevel.Error, "远端预检失败："+outText)
		return out, fmt.Errorf("remote preflight failed: %s", outText)
	}
	lines := strings.Split(outText, "\n")
	if len(lines) < 2 {
		return out, fmt.Errorf("remote preflight returned no platform: %q", outText)
	}
	osName, arch := normalizeUname(lines[len(lines)-2], lines[len(lines)-1])
	out.OS, out.Arch = osName, arch
	out.platform = osName + "/" + arch
	_ = store.AddDeployEvent(job.RequestID, eventlevel.Success,
		"远端预检通过：ssh "+target.SSHDest()+" 平台 "+out.platform+"")
	return out, nil
}

// remoteDirCheck 是第二段：目标 runtime 目录存在且像这个服务（没有 scripts/restart.sh 就
// 拒绝 rsync --delete，不会误删；不存在则创建、首次部署不带 --delete）。
func remoteDirCheck(store *Store, job DeployJob, target MachineTarget, remote RemoteRunner,
	runtimeDir string) (int, string) {
	check := strings.Join([]string{
		"set -e",
		"if [ -d " + shellQuote(runtimeDir) + " ]; then",
		"  [ -f " + shellQuote(runtimeDir+"/scripts/restart.sh") + " ] || { echo " +
			shellQuote(runtimeDir+" exists but is not this service's runtime (no scripts/restart.sh); refusing to rsync --delete") + "; exit 4; }",
		"else",
		"  mkdir -p " + shellQuote(runtimeDir),
		"  echo 'runtime dir created (first deploy)'",
		"fi",
	}, "\n")
	code, out := remote.Run(target, check, 60)
	out = strings.TrimSpace(out)
	if code != 0 {
		_ = store.AddDeployEvent(job.RequestID, eventlevel.Error, "远端 runtime 目录检查失败："+out)
		return code, "remote runtime dir check failed: " + out
	}
	_ = store.AddDeployEvent(job.RequestID, eventlevel.Success,
		"远端 runtime 目录就绪："+runtimeDir+" "+out)
	return 0, ""
}

// remotePlatformGuard 在推送前比对包与远端的平台：不一致直接失败（不落地、不重启）。
func remotePlatformGuard(store *Store, job DeployJob, src, remotePlatform string) error {
	if remotePlatform == "" {
		return nil
	}
	parts := strings.SplitN(remotePlatform, "/", 2)
	if len(parts) != 2 {
		return nil
	}
	if err := assertPackageMatchesRemote(src, parts[0], parts[1]); err != nil {
		_ = store.AddDeployEvent(job.RequestID, eventlevel.Error, "产物平台检查失败："+err.Error())
		return err
	}
	_ = store.AddDeployEvent(job.RequestID, eventlevel.Success,
		"产物平台检查通过：包里二进制与远端 "+remotePlatform+" 一致")
	return nil
}

// remoteEnvPrefix 是远端执行前的那段 `cd <runtime> && export …`（restart= 覆盖时复用）。
func remoteEnvPrefix(service ServiceContract, extra map[string]string) string {
	env := map[string]string{"SERVICE_PORT": servicePort(service), "PORT": servicePort(service)}
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
	cmd := "export " + strings.Join(assign, " ")
	if dir := strings.TrimSpace(extra["RUNTIME_DIR"]); dir != "" {
		cmd = "cd " + shellQuote(dir) + " && " + cmd
	}
	return cmd
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
