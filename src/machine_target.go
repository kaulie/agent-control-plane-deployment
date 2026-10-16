package main

import (
	"fmt"
	"path"
	"regexp"
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
	// RestartCmd 覆盖契约里的 restartCmd。一条通道经常被这台机器上的多个服务共用，所以
	// 不是无条件套用（见 restartOverrideApplies）：
	//   - 命令含 {serviceId}，或没有点名具体 systemd 单元 → 每个服务都用它；
	//   - 命令点名了具体单元（systemctl restart autonomyd，或 autonomyd.service 路径）→
	//     只对单元名里包含该 serviceId 的服务生效。autonomyd 给 autonomy，不会在部署
	//     knowledge-base 时把 autonomyd 重启一遍、再拿 knowledge-base 的端口探活。
	// 支持占位符：{serviceId} {version} {runtimeDir} {remoteDir} {machine}。
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

// systemctl 带单元的子命令。daemon-reload 这类不带单元的不在这里。
var systemctlUnitRe = regexp.MustCompile(`\bsystemctl\s+(?:(?:--\S+)\s+)*(?:restart|start|stop|reload|try-restart|reload-or-restart|enable|disable)\s+(\S+)`)

// unit 文件路径：/etc/systemd/system/autonomyd.service 或 …/autonomyd.service.d/…
var systemdUnitFileRe = regexp.MustCompile(`(?:^|[^\w.@-])([\w.@-]+)\.service(?:\.d)?(?:[^\w.@-]|$)`)

// systemdUnitNames 抽出命令里点名的 systemd 单元（去掉 .service 后缀与引号）。
func systemdUnitNames(cmd string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(raw string) {
		u := strings.Trim(strings.TrimSpace(raw), `"'`)
		u = strings.TrimSuffix(u, ".service")
		if u == "" || strings.HasPrefix(u, "-") || seen[u] {
			return
		}
		seen[u] = true
		out = append(out, u)
	}
	for _, m := range systemctlUnitRe.FindAllStringSubmatch(cmd, -1) {
		add(m[1])
	}
	for _, m := range systemdUnitFileRe.FindAllStringSubmatch(cmd, -1) {
		add(m[1])
	}
	return out
}

// foreignRestartUnits 是这条 restart= 点名、但并不属于 serviceID 的单元。
// 命令含 {serviceId} 时视为每个服务的模板，不把任何单元当成「别人的」。
func foreignRestartUnits(rawCmd, serviceID string) []string {
	rawCmd = strings.TrimSpace(rawCmd)
	if rawCmd == "" || strings.Contains(rawCmd, "{serviceId}") {
		return nil
	}
	var foreign []string
	for _, unit := range systemdUnitNames(rawCmd) {
		if serviceID == "" || !strings.Contains(unit, serviceID) {
			foreign = append(foreign, unit)
		}
	}
	return foreign
}

// restartOverrideApplies 判断通道 restart= 能不能用在这次部署的服务上。
// 空命令不适用。没有点名单元、或写了 {serviceId}、或每个单元名都包含 serviceId → 适用。
func restartOverrideApplies(rawCmd, serviceID string) bool {
	if strings.TrimSpace(rawCmd) == "" {
		return false
	}
	return len(foreignRestartUnits(rawCmd, serviceID)) == 0
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
		// 平台的唯一来源是注册中心：实例的 os + arch，或老的 metadata.platform。
		return target, fmt.Errorf("%s 的 %q：不要在部署通道里写 platform= —— 机器的平台由"+
			"service_registry 上该机器的实例登记（os + arch，或 metadata.platform，如 linux/amd64），控制面同步它",
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
