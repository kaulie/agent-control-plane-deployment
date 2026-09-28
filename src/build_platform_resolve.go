package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// 目标机器的平台怎么定（打包与部署都要用）：
//
//	① 部署通道里显式写了 platform=linux/amd64 → 直接用它（不必 ssh）；
//	② 远端机器 → ssh 跑 uname -s / uname -m 探（结果按机器缓存，避免每次都出网）；
//	③ 本机 → 控制面自己的 GOOS/GOARCH。
//
// 探不到（ssh 不通等）时返回本机平台 + 原因：打包先按本机来，部署前的平台校验会兜底。
type platformCacheEntry struct {
	platform BuildPlatform
	at       time.Time
}

type platformResolver struct {
	mu     sync.Mutex
	cache  map[string]platformCacheEntry
	ttl    time.Duration
	remote RemoteRunner
}

func newPlatformResolver(remote RemoteRunner) *platformResolver {
	if remote == nil {
		remote = sshRemoteRunner{}
	}
	return &platformResolver{cache: map[string]platformCacheEntry{}, ttl: 5 * time.Minute, remote: remote}
}

// Resolve 返回这台机器上产物应当构建的平台（+ 判定依据，写进时间线）。
func (r *platformResolver) Resolve(target MachineTarget) (BuildPlatform, string) {
	if p := target.BuildPlatformFromTarget(); !p.IsZero() {
		return p, "通道里声明 platform=" + p.String()
	}
	if !target.Remote() {
		return LocalBuildPlatform(), "本机平台"
	}
	r.mu.Lock()
	if e, ok := r.cache[target.ID]; ok && time.Since(e.at) < r.ttl {
		r.mu.Unlock()
		return e.platform, "远端平台（探测缓存）"
	}
	r.mu.Unlock()

	code, out := r.remote.Run(target, "uname -s; uname -m", 30)
	if code != 0 {
		return LocalBuildPlatform(), fmt.Sprintf("探测 %s 平台失败（%s），先按本机平台", target.ID, firstLine(out))
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return LocalBuildPlatform(), "探测输出无法解析，先按本机平台"
	}
	osName, arch := normalizeUname(lines[len(lines)-2], lines[len(lines)-1])
	p := BuildPlatform{OS: osName, Arch: arch}
	if p.IsZero() {
		return LocalBuildPlatform(), "探测输出无法解析，先按本机平台"
	}
	r.mu.Lock()
	r.cache[target.ID] = platformCacheEntry{platform: p, at: time.Now()}
	r.mu.Unlock()
	return p, "远端平台（ssh uname 探测）"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// targetForPackaging 是打包前的便捷入口：拿到机器通道 + 该机器的构建平台。
func targetForPackaging(ctx context.Context, cat *MachineCatalog, resolver *platformResolver, machine string) (MachineTarget, BuildPlatform, string, error) {
	if strings.TrimSpace(machine) == "" {
		machine = defaultDeployMachine
	}
	if resolver == nil {
		resolver = newPlatformResolver(nil)
	}
	target, ok := cat.Target(ctx, machine)
	if !ok {
		return MachineTarget{}, BuildPlatform{}, "", fmt.Errorf("部署机器 %q 没有部署通道", machine)
	}
	p, why := resolver.Resolve(target)
	return target, p, why, nil
}
