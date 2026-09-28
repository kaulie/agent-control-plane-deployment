package main

import (
	"encoding/json"
	"runtime"
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

// RuntimeDirForRemote 是**远端**部署时的目录优先级：
//
//  1. 契约里该平台的显式配置（runtimeDirs.linux）—— 用户说的「分平台路径」；
//  2. 部署通道约定的 <remote-runtime-home>/<serviceId>（那台机器自己的布局）；
//  3. 契约默认 runtimeDir（最后兜底：只有契约没分平台、通道也没给家目录时才用）。
//
// 顺序有意如此：默认 runtimeDir 是「本机」语义的路径（/Users/...），直接拿去远端几乎一定是错的，
// 所以先让机器自己的约定说了算，只有在没别的依据时才回落到它。
func (c ServiceContract) RuntimeDirForRemote(goos, serviceID string, target MachineTarget) string {
	if dir, ok := c.PlatformRuntimeDir(goos); ok {
		return dir
	}
	if dir := target.RuntimeDirFor(serviceID); dir != "" {
		return dir
	}
	return strings.TrimSpace(c.RuntimeDir)
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
