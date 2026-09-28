package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kaulie/agent-control-plane-deployment/eventlevel"
)

// 产物平台：tag 带平台维度、打包按目标平台构建、部署前校验平台一致。

func TestDeploymentTagPlatformDimension(t *testing.T) {
	local := LocalBuildPlatform()
	// 本机平台（或未指定）→ 保持老 tag（兼容线上已有制品）
	if got := deploymentTagFor("abc12345", BuildPlatform{}); got != "deployment-abc12345" {
		t.Fatalf("zero platform tag = %q", got)
	}
	if got := deploymentTagFor("abc12345", local); got != "deployment-abc12345" {
		t.Fatalf("local platform tag = %q, want the legacy form", got)
	}
	// 别的平台 → 带平台维度（同一 commit 的两份产物不互相覆盖）
	other := BuildPlatform{OS: "linux", Arch: "amd64"}
	if local.OS == "linux" {
		other = BuildPlatform{OS: "darwin", Arch: "amd64"}
	}
	got := deploymentTagFor("abc12345", other)
	want := "deployment-abc12345-" + other.OS + "-" + other.Arch
	if got != want {
		t.Fatalf("cross-platform tag = %q, want %q", got, want)
	}
	// 解析回来
	hash, platform := ParseDeploymentTag(got)
	if hash != "abc12345" || platform != other {
		t.Fatalf("ParseDeploymentTag(%q) = (%q, %v)", got, hash, platform)
	}
	if hash, platform := ParseDeploymentTag("deployment-abc12345"); hash != "abc12345" || !platform.IsZero() {
		t.Fatalf("legacy tag parse = (%q, %v)", hash, platform)
	}
	if got := deploymentHash("deployment-abc12345-linux-amd64"); got != "abc12345" {
		t.Fatalf("deploymentHash = %q", got)
	}
}

func TestParseBuildPlatform(t *testing.T) {
	cases := map[string]BuildPlatform{
		"linux/amd64": {OS: "linux", Arch: "amd64"},
		"linux-amd64": {OS: "linux", Arch: "amd64"},
		"linux_arm64": {OS: "linux", Arch: "arm64"},
		"macos":       {OS: "darwin", Arch: LocalBuildPlatform().Arch},
	}
	for in, want := range cases {
		got, err := ParseBuildPlatform(in)
		if err != nil || got != want {
			t.Fatalf("ParseBuildPlatform(%q) = (%v, %v), want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"windows/amd64", "linux/sparc", "solaris"} {
		if _, err := ParseBuildPlatform(bad); err == nil {
			t.Fatalf("ParseBuildPlatform(%q) should fail", bad)
		}
	}
}

// 打包时按目标平台构建：build.sh 拿到 GOOS/GOARCH，tag 带平台，包里有 PLATFORM。
func TestPackageFromGitBuildsForTargetPlatform(t *testing.T) {
	workdir := t.TempDir()
	repo := filepath.Join(workdir, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	// 一个极小的「服务仓库」：build.sh 把 GOOS/GOARCH 写进 outputs/，方便断言。
	buildSh := "#!/usr/bin/env bash\nset -euo pipefail\nmkdir -p outputs\necho \"${GOOS:-$(go env GOOS 2>/dev/null || echo none)}/${GOARCH:-none}\" > outputs/PLATFORM-PROBE\necho ok > outputs/PAYLOAD\n"
	mustWrite(t, filepath.Join(repo, "build.sh"), buildSh, 0o755)
	mustWrite(t, filepath.Join(repo, "README.md"), "fixture\n", 0o644)
	runGit(t, repo, "init", "-q", "-b", "main")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "-qm", "init")

	platform := BuildPlatform{OS: "linux", Arch: "amd64"}
	if LocalBuildPlatform() == platform {
		platform = BuildPlatform{OS: "darwin", Arch: "amd64"}
	}
	pkgs := filepath.Join(workdir, "packages")
	storage := &localStorage{base: pkgs}
	var events []string
	res, err := packageFromGit("svc-x", "file://"+repo, "main", PackageOptions{
		MaxSec:        120,
		Storage:       storage,
		BuildPlatform: platform,
		Events: func(_ eventlevel.Level, msg string) {
			events = append(events, msg)
		},
	})
	if err != nil {
		t.Fatalf("packageFromGit: %v", err)
	}
	if !strings.HasSuffix(res.Tag, "-"+platform.OS+"-"+platform.Arch) {
		t.Fatalf("tag = %q, want the platform suffix", res.Tag)
	}
	if res.Platform != platform.String() {
		t.Fatalf("result platform = %q", res.Platform)
	}
	// build.sh 真的拿到了目标平台的 GOOS/GOARCH
	probe, err := os.ReadFile(filepath.Join(pkgs, "svc-x", res.Tag, "PLATFORM-PROBE"))
	if err != nil {
		t.Fatalf("read probe (artifact must exist under the platform tag): %v", err)
	}
	if strings.TrimSpace(string(probe)) != platform.String() {
		t.Fatalf("build.sh saw GOOS/GOARCH = %q, want %q", strings.TrimSpace(string(probe)), platform)
	}
	// 包里带 PLATFORM，部署前据此判断
	if b, err := os.ReadFile(filepath.Join(pkgs, "svc-x", res.Tag, "PLATFORM")); err != nil || strings.TrimSpace(string(b)) != platform.String() {
		t.Fatalf("package PLATFORM = %q err=%v", strings.TrimSpace(string(b)), err)
	}
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, "按目标平台构建：GOOS="+platform.OS) {
		t.Fatalf("timeline should say it builds for the target platform, got:\n%s", joined)
	}
}

func TestAssertTagPlatformMatchesMachine(t *testing.T) {
	local := LocalBuildPlatform()
	remote := MachineTarget{ID: "m", Kind: "ssh", SSHHost: "host"}
	linuxPlatform := "linux/amd64"
	if local.OS == "linux" && local.Arch == "amd64" {
		linuxPlatform = "darwin/amd64"
	}

	// 本机 tag + 本机机器 → 通过
	if err := assertTagPlatformMatchesMachine(deploymentTagFor("abc12345", local), "local", MachineTarget{ID: "local", Kind: "local"}, ""); err != nil {
		t.Fatalf("local tag on local machine must pass: %v", err)
	}
	// 跨平台 tag + 对应平台的远端 → 通过
	parts := strings.SplitN(linuxPlatform, "/", 2)
	crossTag := deploymentTagFor("abc12345", BuildPlatform{OS: parts[0], Arch: parts[1]})
	if err := assertTagPlatformMatchesMachine(crossTag, "m", remote, linuxPlatform); err != nil {
		t.Fatalf("matching remote platform must pass: %v", err)
	}
	// 本机 tag 发到别的平台远端 → 拒绝，并说清怎么办
	err := assertTagPlatformMatchesMachine(deploymentTagFor("abc12345", local), "m", remote, linuxPlatform)
	if err == nil {
		t.Fatal("a local-platform artifact must not be deployed to a foreign machine")
	}
	for _, want := range []string{"制品平台与机器不符", "未下载/未推送", "platform="} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should contain %q, got %v", want, err)
		}
	}
}

func mustWrite(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
