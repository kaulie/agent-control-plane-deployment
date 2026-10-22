package main

import (
	"context"
	"encoding/json"
	"path"
	"runtime"
	"sort"
	"strings"
)

// runtimeDir 支持按平台配置不同的路径 —— 同一个服务在本机（macOS）与 Linux 机器上
// 往往落在不同目录（/Users/gaolei/runtime/x vs /home/ubuntu/runtime/x）：
//
//	runtimeDir       默认（兜底；老契约就只有它）
//	runtimeDirs      { "darwin": "...", "linux": "..." } 按目标机器平台覆盖
//
// 部署时按**目标机器**的平台选（本机 = 控制面自己的 GOOS；远端 = 预检 probe 出来的
// uname），见 ServiceContract.RuntimeDirForOS。

// normalizeRuntimeOS 把平台名归一成 darwin / linux（认得 macOS 的几种写法）。
func normalizeRuntimeOS(goos string) string {
	switch strings.ToLower(strings.TrimSpace(goos)) {
	case "darwin", "macos", "mac", "macosx", "osx":
		return "darwin"
	case "linux", "gnu/linux":
		return "linux"
	}
	return strings.ToLower(strings.TrimSpace(goos))
}

// PlatformRuntimeDir 返回 runtimeDirs 里针对该平台的**显式**配置（ok=false = 没配这个平台）。
func (c ServiceContract) PlatformRuntimeDir(goos string) (string, bool) {
	if len(c.RuntimeDirs) == 0 {
		return "", false
	}
	want := normalizeRuntimeOS(goos)
	for k, v := range c.RuntimeDirs {
		if strings.TrimSpace(v) == "" {
			continue
		}
		if normalizeRuntimeOS(k) == want {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}

// RuntimeDirForOS 返回某平台上这个服务的 runtime 目录：runtimeDirs[平台] 优先，
// 其次默认 runtimeDir；两者都没有 → 空串（调用方报错）。
func (c ServiceContract) RuntimeDirForOS(goos string) string {
	if dir, ok := c.PlatformRuntimeDir(goos); ok {
		return dir
	}
	return strings.TrimSpace(c.RuntimeDir)
}

// RuntimeDirForRemote 是**远端**部署时的目录。
//
// Linux：只认配置页上提前填好的 runtimeDirs.linux。通道的 <remote-home>/<serviceId>
// 和本机默认 runtimeDir 都是隐含路径，不能拿去 Linux 机器。没配就返回空串，调用方拒绝部署。
// 其它平台：该平台的显式路径优先，其次通道约定，最后才是默认 runtimeDir。
func (c ServiceContract) RuntimeDirForRemote(goos, serviceID string, target MachineTarget) string {
	if normalizeRuntimeOS(goos) == "linux" {
		if dir, ok := c.PlatformRuntimeDir("linux"); ok {
			return dir
		}
		return ""
	}
	if dir, ok := c.PlatformRuntimeDir(goos); ok {
		return dir
	}
	if dir := target.RuntimeDirFor(serviceID); dir != "" {
		return dir
	}
	return strings.TrimSpace(c.RuntimeDir)
}

// runtimeDirReadyGate 在部署开始前，按配置中心实例的 os 判断目录是否已经配完整。
// os=linux 只认配置页上的绝对路径「runtimeDir（Linux 机器）」；os=mac 认本机/macOS 目录。
// 实例没有 os（老注册中心）时不在这里拦，打包前的探测和部署预检再查。
func runtimeDirReadyGate(ctx context.Context, cat *MachineCatalog, svc *ServiceContract, machine string) string {
	if cat == nil || svc == nil {
		return ""
	}
	osName, ok := cat.InstanceOS(ctx, svc.ServiceID, machine)
	if !ok {
		return ""
	}
	return runtimeDirGap(svc, osName)
}

// runtimeDirGap 是某个 OS 还缺的目录说明。目录完整 = 该 OS 对应的配置是绝对路径。
func runtimeDirGap(svc *ServiceContract, osName string) string {
	if svc == nil {
		return ""
	}
	switch normalizeRuntimeOS(osName) {
	case "linux":
		if dir, ok := svc.PlatformRuntimeDir("linux"); ok && absoluteRuntimeDir(dir) {
			return ""
		}
		return missingLinuxRuntimeDirError(svc.ServiceID)
	case "darwin":
		if absoluteRuntimeDir(svc.RuntimeDirForOS("darwin")) {
			return ""
		}
		return missingDarwinRuntimeDirError(svc.ServiceID)
	default:
		return ""
	}
}

// absoluteRuntimeDir 判断目录是不是绝对路径（完整，能直接落到目标机器上）。
func absoluteRuntimeDir(dir string) bool {
	dir = strings.TrimSpace(dir)
	return strings.HasPrefix(dir, "/") || strings.HasPrefix(dir, "~")
}

// missingLinuxRuntimeDirError 告诉操作者去配置页填 Linux 目录。目录只认这一项。
func missingLinuxRuntimeDirError(serviceID string) string {
	id := strings.TrimSpace(serviceID)
	if id == "" {
		id = "该服务"
	}
	return "部署到 Linux 机器前，必须在部署配置页填好「runtimeDir（Linux 机器）」为绝对路径" +
		"（/panel/service-edit.html?serviceId=" + id + "）。目录只认实例 OS 对应的这一项"
}

func missingDarwinRuntimeDirError(serviceID string) string {
	id := strings.TrimSpace(serviceID)
	if id == "" {
		id = "该服务"
	}
	return "部署到 macOS 机器前，必须在部署配置页填好「runtimeDir（默认：本机平台用）」或「runtimeDir（macOS 机器）」为绝对路径" +
		"（/panel/service-edit.html?serviceId=" + id + "）"
}

// LocalRuntimeDir 是本机（控制面所在平台）上这个服务的 runtime 目录。
func (c ServiceContract) LocalRuntimeDir() string {
	return c.RuntimeDirForOS(runtime.GOOS)
}

// NormalizeRuntimeDirs 归一 runtimeDirs 的键名并丢掉空值；返回归一后的 map（无内容 → nil）。
func NormalizeRuntimeDirs(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, v := range in {
		key := normalizeRuntimeOS(k)
		val := strings.TrimSpace(v)
		if key == "" || val == "" {
			continue
		}
		out[key] = val
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// encodeRuntimeDirs / decodeRuntimeDirs 是入库形态（JSON 文本）。
func encodeRuntimeDirs(in map[string]string) string {
	normalized := NormalizeRuntimeDirs(in)
	if len(normalized) == 0 {
		return "{}"
	}
	b, err := json.Marshal(normalized)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func decodeRuntimeDirs(raw string) map[string]string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return nil
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return NormalizeRuntimeDirs(out)
}

// adaptServiceForRuntime 把契约里写死的本机路径改到这次部署实际使用的 runtime 目录。
//
// 配置页上的 startCmd 经常是 `bash "/Users/.../scripts/start.sh"`，而 Linux 机器的
// 目录在 runtimeDirs.linux。只 cd 到远端目录不够：命令本身仍是绝对路径，bash 会报
// No such file or directory。运行时环境变量里指向同一台机器家目录的其它路径
// （例如 sslrootcert）一并改写；runtime 目录下的其它服务路径不猜。
func adaptServiceForRuntime(service ServiceContract, targetDir string) ServiceContract {
	targetDir = cleanRuntimePath(targetDir)
	if targetDir == "" {
		return service
	}
	out := service
	out.StartCmd = adaptContractText(service.StartCmd, service, targetDir)
	out.StopCmd = adaptContractText(service.StopCmd, service, targetDir)
	out.RestartCmd = adaptContractText(service.RestartCmd, service, targetDir)
	if len(service.RuntimeEnv) > 0 {
		env := make(map[string]string, len(service.RuntimeEnv))
		for k, v := range service.RuntimeEnv {
			env[k] = adaptContractText(v, service, targetDir)
		}
		out.RuntimeEnv = env
	}
	return out
}

func adaptContractText(text string, service ServiceContract, targetDir string) string {
	if text == "" {
		return text
	}
	out := text
	for _, dir := range configuredRuntimePaths(service) {
		if dir == targetDir {
			continue
		}
		out = replacePathPrefix(out, dir, targetDir, "")
	}
	if from, to, reserved, ok := runtimeRootSwap(service, targetDir); ok {
		out = replacePathPrefix(out, from, to, reserved)
	}
	return out
}

func configuredRuntimePaths(service ServiceContract) []string {
	seen := map[string]bool{}
	dirs := make([]string, 0, 1+len(service.RuntimeDirs))
	add := func(dir string) {
		dir = cleanRuntimePath(dir)
		if dir == "" || dir == "/" || seen[dir] {
			return
		}
		seen[dir] = true
		dirs = append(dirs, dir)
	}
	add(service.RuntimeDir)
	for _, dir := range service.RuntimeDirs {
		add(dir)
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	return dirs
}

func cleanRuntimePath(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ""
	}
	cleaned := path.Clean(dir)
	if cleaned == "." {
		return ""
	}
	return cleaned
}

// runtimeRootSwap 在两条 runtime 目录只差一个根前缀时，给出这个前缀的替换。
//
// /Users/gaolei/runtime/service-registry 与 /home/ubuntu/runtime/service-registry
// → 把 /Users/gaolei 换成 /home/ubuntu。公共后缀至少两段，避免 /opt/web-cursor 这种
// 只同名最后一段的目录把整个家目录换掉。reserved 是公共后缀的第一段（通常是 runtime）：
// 这段下面的路径只做精确目录替换，不用家目录前缀去猜别的服务。
func runtimeRootSwap(service ServiceContract, targetDir string) (from, to, reserved string, ok bool) {
	targetDir = cleanRuntimePath(targetDir)
	if targetDir == "" || targetDir == "/" {
		return "", "", "", false
	}
	bestN := 1
	for _, dir := range configuredRuntimePaths(service) {
		if dir == targetDir {
			continue
		}
		n := commonPathSuffixLen(dir, targetDir)
		if n < 2 || n <= bestN {
			continue
		}
		suf := commonPathSuffix(dir, targetDir)
		f := strings.Trim(strings.TrimSuffix(dir, suf), "/")
		t := strings.Trim(strings.TrimSuffix(targetDir, suf), "/")
		if f == "" || t == "" || f == t {
			continue
		}
		bestN = n
		from = "/" + f
		to = "/" + t
		reserved = strings.Trim(suf, "/")
		if i := strings.IndexByte(reserved, '/'); i >= 0 {
			reserved = reserved[:i]
		}
		ok = true
	}
	return from, to, reserved, ok
}

func commonPathSuffixLen(a, b string) int {
	as := splitRuntimePath(a)
	bs := splitRuntimePath(b)
	n := 0
	for i, j := len(as)-1, len(bs)-1; i >= 0 && j >= 0 && as[i] == bs[j]; i, j = i-1, j-1 {
		n++
	}
	return n
}

func commonPathSuffix(a, b string) string {
	as := splitRuntimePath(a)
	n := commonPathSuffixLen(a, b)
	if n == 0 {
		return ""
	}
	return "/" + strings.Join(as[len(as)-n:], "/")
}

func splitRuntimePath(p string) []string {
	p = cleanRuntimePath(p)
	if p == "" || p == "/" {
		return nil
	}
	return strings.Split(strings.Trim(p, "/"), "/")
}

// replacePathPrefix 替换绝对路径前缀。后一字符必须是路径边界或 /，避免
// service-registry 误伤 service-registry-extra。skipNext 非空时，前缀后面
// 紧跟的那一段目录不替换（留给精确的 runtime 目录替换）。
func replacePathPrefix(s, from, to, skipNext string) string {
	from = cleanRuntimePath(from)
	to = cleanRuntimePath(to)
	if from == "" || from == "/" || from == to || !strings.Contains(s, from) {
		return s
	}
	var b strings.Builder
	rest := s
	for {
		i := strings.Index(rest, from)
		if i < 0 {
			b.WriteString(rest)
			return b.String()
		}
		end := i + len(from)
		beforeOK := i == 0 || isPathDelim(rest[i-1])
		afterOK := end == len(rest) || rest[end] == '/' || isPathDelim(rest[end])
		if beforeOK && afterOK && !skipReservedComponent(rest[end:], skipNext) {
			b.WriteString(rest[:i])
			b.WriteString(to)
			rest = rest[end:]
			continue
		}
		// 滑过这个起点，避免 from 在自身内部反复命中。
		next := i + 1
		if next > len(rest) {
			next = len(rest)
		}
		b.WriteString(rest[:next])
		rest = rest[next:]
	}
}

func skipReservedComponent(after, reserved string) bool {
	if reserved == "" || !strings.HasPrefix(after, "/") {
		return false
	}
	comp := strings.TrimPrefix(after, "/")
	if i := strings.IndexByte(comp, '/'); i >= 0 {
		comp = comp[:i]
	}
	return comp == reserved
}

func isPathDelim(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '"', '\'', '=', ':', ';', '&', '|', '(', ')', '<', '>':
		return true
	default:
		return false
	}
}
