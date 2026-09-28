package main

import (
	"fmt"
	"runtime"
	"strings"
)

// 产物平台：跨机部署要求产物与**目标机器**的平台一致（macOS 上打的 Mach-O 到了 Linux
// 起不来）。所以：
//
//	打包时按目标平台的 GOOS/GOARCH 构建（build.sh 拿到这两个环境变量）；
//	制品 tag 带上平台维度，避免同一个 commit 的两份产物互相覆盖；
//	部署前校验「tag 上的平台 == 机器平台」，不一致直接失败（而不是推过去才发现）。
//
// 本机平台保持老 tag（deployment-<hash>）：线上已有的制品、面板、历史记录都不受影响。
type BuildPlatform struct {
	OS   string // darwin | linux
	Arch string // amd64 | arm64 | …
}

func (p BuildPlatform) IsZero() bool { return p.OS == "" || p.Arch == "" }
func (p BuildPlatform) String() string {
	if p.IsZero() {
		return ""
	}
	return p.OS + "/" + p.Arch
}

// LocalBuildPlatform 是控制面自己所在平台（不指定目标平台时的缺省）。
func LocalBuildPlatform() BuildPlatform {
	return BuildPlatform{OS: runtime.GOOS, Arch: runtime.GOARCH}
}

// IsLocal 判断这个平台是不是控制面本机平台。
func (p BuildPlatform) IsLocal() bool { return p == LocalBuildPlatform() }

// ParseBuildPlatform 解析 `linux/amd64`、`linux-amd64`、`linux_amd64` 或只给 `linux`
// （只给 OS 时按本机架构补齐）。
func ParseBuildPlatform(raw string) (BuildPlatform, error) {
	s := strings.TrimSpace(strings.ToLower(raw))
	if s == "" {
		return BuildPlatform{}, nil
	}
	osName, arch := "", ""
	if i := strings.IndexAny(s, "/_- "); i >= 0 {
		osName, arch = strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:])
	} else {
		osName = s
	}
	osName = normalizeRuntimeOS(osName)
	if osName != "darwin" && osName != "linux" {
		return BuildPlatform{}, fmt.Errorf("不认识的平台 %q（只支持 darwin / linux）", raw)
	}
	if arch == "" {
		arch = LocalBuildPlatform().Arch
	}
	switch arch {
	case "amd64", "arm64", "386", "arm":
	default:
		return BuildPlatform{}, fmt.Errorf("不认识的架构 %q（支持 amd64 / arm64 / 386 / arm）", raw)
	}
	return BuildPlatform{OS: osName, Arch: arch}, nil
}

// deploymentTagFor 是某个 commit + 平台对应的制品 tag：
//
//	本机平台（或未指定平台） → deployment-<hash>            （与历史一致）
//	其它平台                 → deployment-<hash>-<os>-<arch>
func deploymentTagFor(hash string, p BuildPlatform) string {
	hash = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(hash), "deployment-"))
	if p.IsZero() || p.IsLocal() {
		return "deployment-" + hash
	}
	return fmt.Sprintf("deployment-%s-%s-%s", hash, p.OS, p.Arch)
}

// ParseDeploymentTag 把 tag 拆回 (hash, 平台)。
//
//	deployment-abc12345                  → (abc12345, zero)
//	deployment-abc12345-linux-amd64      → (abc12345, linux/amd64)
func ParseDeploymentTag(tag string) (string, BuildPlatform) {
	s := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(tag), "deployment-"))
	parts := strings.Split(s, "-")
	if len(parts) >= 3 {
		// 只认「已知 os + 已知 arch」的尾巴（ParseBuildPlatform 会校验），
		// 这样 hash 里的连字符不会被误当平台。
		if p, err := ParseBuildPlatform(parts[len(parts)-2] + "/" + parts[len(parts)-1]); err == nil && !p.IsZero() {
			return strings.Join(parts[:len(parts)-2], "-"), p
		}
	}
	return s, BuildPlatform{}
}

// deploymentHash 是 tag 里的 commit 短 hash（VERSION 文件内容）。
func deploymentHash(tag string) string {
	hash, _ := ParseDeploymentTag(tag)
	return hash
}

// assertTagPlatformMatchesMachine 校验「制品 tag 上的平台」与目标机器平台一致。
//
//	本机机器：产物必须与本机平台一致；
//	远端机器：产物必须与远端 uname 出来的平台一致。
//
// 不一致时**不下载、不推送**，直接给出补救办法（换机器、或用对应平台重新打包）。
func assertTagPlatformMatchesMachine(tag, machine string, target MachineTarget, remotePlatform string) error {
	_, artPlatform := ParseDeploymentTag(tag)
	if artPlatform.IsZero() {
		// 老 tag（没有平台维度）：视为本机平台产物。
		artPlatform = LocalBuildPlatform()
	}
	want := LocalBuildPlatform()
	if target.Remote() {
		parts := strings.SplitN(strings.TrimSpace(remotePlatform), "/", 2)
		if len(parts) == 2 {
			want = BuildPlatform{OS: parts[0], Arch: parts[1]}
		}
	}
	if want.IsZero() || artPlatform == want {
		return nil
	}
	return fmt.Errorf("制品平台与机器不符：%s 是 %s 产物，机器 %s 是 %s —— 请在**该平台的机器**上触发打包"+
		"（面板选对「部署机器」；通道里也可显式写 platform=%s），本次未下载/未推送任何文件",
		tag, artPlatform, machine, want, want)
}
