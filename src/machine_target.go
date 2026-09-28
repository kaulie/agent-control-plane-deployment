package main

import (
	"fmt"
	"path"
	"strconv"
	"strings"
)

// MachineTarget 说明「怎么把一次部署送到某台机器」—— 机器能不能被选中，就取决于有没有它。
//
// 配置（DEPLOY_MACHINE_TARGETS，或本机 data/machine-targets，见 scripts/start.sh）：
//
//	DEPLOY_MACHINE_TARGETS='local=local; 43.162.117.240=ssh ubuntu@43.162.117.240 /home/ubuntu/runtime'
//
// 一条一个机器，`;`（或换行）分隔，每条是 `id=spec`：
//   - `local`             就本机执行（缺省机器 local 自带这条，不用配）；
//   - `ssh [user@]host[:port] <remote-runtime-home>`
//     远端执行：制品 rsync 到 <remote-runtime-home>/<serviceId>，再用 ssh 在那边跑
//     restartCmd、用远端自己的 127.0.0.1 URL 做 graceful/健康检查。
//
// 只登记在注册中心（或 DEPLOY_MACHINES）里的机器是**发现**，没有通道就不能被选中 ——
// 否则会出现「选得中、却静默部署在本机」。
const deployMachineTargetsEnv = "DEPLOY_MACHINE_TARGETS"

// deployMachinesEnv 是「补充机器」的本地配置（env 或 data/deploy-machines）：给**还没在
// service-registry 登记实例**的机器一个全局入口（服务第一次上某台机器时用）。
const deployMachinesEnv = "DEPLOY_MACHINES"

type MachineTarget struct {
	ID          string
	Kind        string // local | ssh
	SSHUser     string
	SSHHost     string
	SSHPort     int
	RuntimeHome string // 远端：服务 runtime 目录的父目录（服务目录 = <home>/<serviceId>）
	// RestartCmd 覆盖契约里的 restartCmd（对这台机器专用）。远端经常有自己的一套：
	// 例如 systemd 管理的服务要 `sudo systemctl restart autonomyd`，还要顺手更新 unit 里的
	// APP_VERSION。支持占位符：{serviceId} {version} {runtimeDir} {remoteDir} {machine}。
	// 形如 `id=ssh user@host /remote/home restart=<命令到条目结尾>`。
	RestartCmd string
}

func (t MachineTarget) Remote() bool { return t.Kind == "ssh" }

// SSHDest 是 ssh/rsync 的目标（user@host）；端口单独用 -p 传。
func (t MachineTarget) SSHDest() string {
	if t.SSHUser != "" {
		return t.SSHUser + "@" + t.SSHHost
	}
	return t.SSHHost
}

// RuntimeDirFor 返回某个服务在这台机器上的 runtime 目录（远端按约定 = <home>/<serviceId>）。
func (t MachineTarget) RuntimeDirFor(serviceID string) string {
	if !t.Remote() || t.RuntimeHome == "" {
		return ""
	}
	return path.Join(strings.TrimSuffix(t.RuntimeHome, "/"), serviceID)
}

// ExpandRestartCmd 展开 restart= 命令里的占位符。
func (t MachineTarget) ExpandRestartCmd(serviceID, version, runtimeDir, remoteDir, machine string) string {
	out := t.RestartCmd
	for k, v := range map[string]string{
		"{serviceId}":  serviceID,
		"{version}":    version,
		"{runtimeDir}": runtimeDir,
		"{remoteDir}":  remoteDir,
		"{machine}":    machine,
	} {
		out = strings.ReplaceAll(out, k, v)
	}
	return strings.TrimSpace(out)
}

// Describe 给事件/日志用的一句话。
func (t MachineTarget) Describe() string {
	if !t.Remote() {
		return t.ID + "（本机）"
	}
	dest := t.SSHDest()
	if t.SSHPort > 0 {
		dest += ":" + strconv.Itoa(t.SSHPort)
	}
	return t.ID + "（远端 ssh " + dest + " → " + t.RuntimeHome + "/<serviceId>）"
}

// ParseMachineTargets 解析 DEPLOY_MACHINE_TARGETS。返回的 map 一定包含 defaultDeployMachine
// （local），除非配置里显式写了别的 local 目标。
func ParseMachineTargets(raw string) (map[string]MachineTarget, error) {
	out := map[string]MachineTarget{}
	for _, entry := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ';' || r == '\n' || r == '\r'
	}) {
		entry = strings.TrimSpace(entry)
		if entry == "" || strings.HasPrefix(entry, "#") {
			continue
		}
		i := strings.IndexByte(entry, '=')
		if i <= 0 {
			return nil, fmt.Errorf("%s 条目 %q 不是 id=spec 形式", deployMachineTargetsEnv, entry)
		}
		id := strings.TrimSpace(entry[:i])
		target, err := parseMachineTargetSpec(id, strings.TrimSpace(entry[i+1:]))
		if err != nil {
			return nil, err
		}
		out[id] = target
	}
	// 本机永远可用（缺省部署目标就是它）。
	if _, ok := out[defaultDeployMachine]; !ok {
		out[defaultDeployMachine] = MachineTarget{ID: defaultDeployMachine, Kind: "local"}
	}
	return out, nil
}

// parseMachineTargetSpec 解析单条目标：`local` 或 `ssh [user@]host[:port] <remote-home>`。
func parseMachineTargetSpec(id, spec string) (MachineTarget, error) {
	target := MachineTarget{ID: id}
	fields := strings.Fields(spec)
	if len(fields) == 0 {
		return target, fmt.Errorf("%s 的 %q 没有目标（要 `local` 或 `ssh [user@]host[:port] <remote-home>`）", deployMachineTargetsEnv, id)
	}
	// 可选尾巴：restart=<命令…>（命令里可以有空格，一直吃到条目结尾）。
	restartCmd := ""
	if i := strings.Index(spec, "restart="); i >= 0 {
		restartCmd = strings.TrimSpace(spec[i+len("restart="):])
		spec = strings.TrimSpace(spec[:i])
	}
	if i := strings.Index(spec, "platform="); i >= 0 {
		// 平台的唯一来源是注册中心：机器在注册中心登记的实例 metadata.platform。
		return target, fmt.Errorf("%s 的 %q：不要在部署通道里写 platform= —— 机器的平台由"+
			"service_registry 上该机器的实例登记（metadata.platform，如 linux/amd64），控制面同步它",
			deployMachineTargetsEnv, id)
	}
	fields = strings.Fields(spec)
	if len(fields) == 0 {
		return target, fmt.Errorf("%s 的 %q 只写了覆盖项没有目标", deployMachineTargetsEnv, id)
	}
	target.RestartCmd = restartCmd
	switch strings.ToLower(fields[0]) {
	case "local":
		if len(fields) != 1 {
			return target, fmt.Errorf("%s 的 %q：local 只接受 `local`（可选 restart=…）", deployMachineTargetsEnv, id)
		}
		target.Kind = "local"
		return target, nil
	case "ssh":
		if len(fields) != 3 {
			return target, fmt.Errorf("%s 的 %q 需要 `ssh [user@]host[:port] <remote-home>`（远端 runtime 目录的父目录）", deployMachineTargetsEnv, id)
		}
		host, port, err := splitSSHHost(fields[1])
		if err != nil {
			return target, fmt.Errorf("%s 的 %q：%w", deployMachineTargetsEnv, id, err)
		}
		home := strings.TrimSpace(fields[2])
		if !strings.HasPrefix(home, "/") && !strings.HasPrefix(home, "~") {
			return target, fmt.Errorf("%s 的 %q：远端 runtime 目录要写绝对路径（如 /home/ubuntu/runtime），得到 %q",
				deployMachineTargetsEnv, id, home)
		}
		target.Kind = "ssh"
		target.SSHUser = host.user
		target.SSHHost = host.host
		target.SSHPort = port
		target.RuntimeHome = home
		return target, nil
	}
	return target, fmt.Errorf("%s 的 %q 目标类型 %q 不认识（只支持 local / ssh）", deployMachineTargetsEnv, id, fields[0])
}

type sshHost struct {
	user string
	host string
}

// splitSSHHost 拆 `[user@]host[:port]`。端口非法/缺主机名都算错。
func splitSSHHost(raw string) (sshHost, int, error) {
	if raw == "" {
		return sshHost{}, 0, fmt.Errorf("目标为空")
	}
	res := sshHost{}
	if i := strings.IndexByte(raw, '@'); i >= 0 {
		res.user = strings.TrimSpace(raw[:i])
		raw = strings.TrimSpace(raw[i+1:])
	}
	port := 0
	if i := strings.LastIndexByte(raw, ':'); i >= 0 {
		p, err := strconv.Atoi(strings.TrimSpace(raw[i+1:]))
		if err != nil || p <= 0 || p > 65535 {
			return sshHost{}, 0, fmt.Errorf("端口 %q 非法", raw[i+1:])
		}
		port = p
		raw = strings.TrimSpace(raw[:i])
	}
	res.host = raw
	if res.host == "" {
		return sshHost{}, 0, fmt.Errorf("缺少主机名")
	}
	return res, port, nil
}
