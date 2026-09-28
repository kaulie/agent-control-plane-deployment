package main

import (
	"debug/elf"
	"debug/macho"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 产物平台检查：跨机部署最常见的事故是把**本机构建**的产物推到另一台机器 ——
// macOS 上打的包（Mach-O）到了 Linux 远端根本起不来，而那时候制品已经推过去、
// 远端服务已经停了。所以在推之前就比一比：包里的可执行文件是什么平台、远端又是什么
// 平台，不一致直接失败（并说清怎么办），绝不落地。

// localBinaryPlatform 返回单个可执行文件的 (os, arch)：linux / darwin，amd64 / arm64 …
func localBinaryPlatform(path string) (string, string) {
	if f, err := elf.Open(path); err == nil {
		defer f.Close()
		return "linux", elfArch(f.Machine)
	}
	if f, err := macho.Open(path); err == nil {
		defer f.Close()
		return "darwin", machoArch(f.Cpu)
	}
	return "", ""
}

func elfArch(m elf.Machine) string {
	switch m {
	case elf.EM_X86_64:
		return "amd64"
	case elf.EM_AARCH64:
		return "arm64"
	case elf.EM_386:
		return "386"
	}
	return strings.ToLower(m.String())
}

func machoArch(c macho.Cpu) string {
	switch c {
	case macho.CpuAmd64:
		return "amd64"
	case macho.CpuArm64:
		return "arm64"
	case macho.Cpu386:
		return "386"
	}
	return strings.ToLower(c.String())
}

// packageBinaries 找包里的可执行文件及其平台（只看 bin/ 与根目录，够用且快）。
func packageBinaries(dir string) map[string]string {
	out := map[string]string{}
	for _, sub := range []string{"bin", "."} {
		entries, err := os.ReadDir(filepath.Join(dir, sub))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			full := filepath.Join(dir, sub, e.Name())
			if st, err := os.Stat(full); err != nil || st.Size() < 64 || st.Mode()&0o111 == 0 {
				continue
			}
			osName, arch := localBinaryPlatform(full)
			if osName == "" {
				continue // 脚本/文本（如 .mjs、.sh）不参与平台判定
			}
			out[filepath.Join(sub, e.Name())] = osName + "/" + arch
		}
	}
	return out
}

// normalizeUname 把远端 uname -s/-m 归一成 GOOS/GOARCH 的说法。
func normalizeUname(osName, arch string) (string, string) {
	switch strings.ToLower(strings.TrimSpace(osName)) {
	case "linux":
		osName = "linux"
	case "darwin":
		osName = "darwin"
	}
	switch strings.ToLower(strings.TrimSpace(arch)) {
	case "x86_64", "amd64":
		arch = "amd64"
	case "aarch64", "arm64":
		arch = "arm64"
	case "i386", "i686":
		arch = "386"
	}
	return strings.ToLower(strings.TrimSpace(osName)), strings.ToLower(strings.TrimSpace(arch))
}

// assertPackageMatchesRemote 比对包内二进制平台与远端平台；不一致就返回错误（带怎么办）。
func assertPackageMatchesRemote(dir string, remoteOS, remoteArch string) error {
	bins := packageBinaries(dir)
	if len(bins) == 0 {
		return nil // 纯脚本包：没有平台约束
	}
	remoteOS, remoteArch = normalizeUname(remoteOS, remoteArch)
	if remoteOS == "" || remoteArch == "" {
		return fmt.Errorf("无法确定远端平台（uname 返回空）")
	}
	bad := []string{}
	for name, plat := range bins {
		if plat != remoteOS+"/"+remoteArch {
			bad = append(bad, name+"="+plat)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	return fmt.Errorf(
		"产物平台与远端不符：%s 是 %s，远端是 %s/%s —— 跨平台部署需要**在目标平台构建**（或在构建机交叉编译）后打包，"+
			"例如 `GOOS=%s GOARCH=%s ./build.sh`；本次未推送任何文件",
		strings.Join(bad, ", "), strings.SplitN(bad[0], "=", 2)[1], remoteOS, remoteArch, remoteOS, remoteArch)
}

// assertPackageMatchesPlatform 校验**打包出来的包**里的可执行文件与目标平台一致。
//
// 典型场景：服务的 build.sh 在构建机（macOS）上按 uname 下载了平台相关的工具，于是
// Linux 包里混进 Mach-O —— 这类包推到远端根本起不来。这里点名是哪个文件，并指出
// 服务的 build.sh 应当按 GOOS/GOARCH 产对应平台的产物（打包阶段失败 = 不上传、不下载）。
func assertPackageMatchesPlatform(dir string, platform BuildPlatform) error {
	if platform.IsZero() {
		return nil
	}
	bins := packageBinaries(dir)
	want := platform.String()
	bad := []string{}
	for name, plat := range bins {
		if plat != want {
			bad = append(bad, name+"="+plat)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	return fmt.Errorf("产物里有非 %s 的二进制：%s —— 服务的 build.sh 需要按 GOOS/GOARCH 产对应平台的产物"+
		"（例如平台相关的下载物也要按目标平台取）；本次未上传制品", want, strings.Join(bad, ", "))
}
