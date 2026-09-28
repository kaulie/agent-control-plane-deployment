package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// 跨机部署的安全网：不许覆盖远端运行态、不许把错平台的产物推过去、
// 并且允许每台机器覆盖 restart 命令（例如 systemd 管理的远端）。

func TestRemoteRsyncKeepsTargetRuntimeState(t *testing.T) {
	target := MachineTarget{ID: "43.162.117.240", Kind: "ssh", SSHHost: "agent-oversea", RuntimeHome: "/home/ubuntu/runtime"}
	cmd := remoteRsyncCmd(target, "/tmp/pkg", "/home/ubuntu/runtime/autonomy", true)
	// 关键豁免必须同时有 P（防删）与 --exclude（防覆盖）。
	for _, keep := range []string{"backend/.env", "backend/data/", "data/", "logs/", "backend/server.log", "backend/runtime.pid"} {
		if !strings.Contains(cmd, "--filter='P "+keep+"'") {
			t.Fatalf("rsync must protect %q from deletion:\n%s", keep, cmd)
		}
		if !strings.Contains(cmd, "--exclude='"+keep+"'") {
			t.Fatalf("rsync must EXCLUDE %q from the transfer (P alone does not stop an overwrite):\n%s", keep, cmd)
		}
	}
	if !strings.Contains(cmd, "--delete") || !strings.Contains(cmd, "agent-oversea:/home/ubuntu/runtime/autonomy/") {
		t.Fatalf("rsync must target the remote runtime dir with --delete:\n%s", cmd)
	}
}

func TestPackagePlatformGuardRejectsForeignBinaries(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 拿当前测试进程自己的可执行文件当样本：它是**本机平台**的二进制。
	self, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Skipf("read self: %v", err)
	}
	sample := filepath.Join(binDir, "autonomyd")
	if err := os.WriteFile(sample, data, 0o755); err != nil {
		t.Fatal(err)
	}

	hostOS, hostArch := runtime.GOOS, runtime.GOARCH
	hostLabel := hostOS + "/" + hostArch
	got, gotArch := localBinaryPlatform(sample)
	hmm := hostOS
	if hmm == "darwin" {
		hmm = "darwin"
	}
	if got != hmm || gotArch != hostArch {
		t.Fatalf("localBinaryPlatform = %s/%s, want %s", got, gotArch, hostLabel)
	}
	bins := packageBinaries(dir)
	if bins["bin/autonomyd"] != hostLabel {
		t.Fatalf("packageBinaries = %v, want bin/autonomyd=%s", bins, hostLabel)
	}

	// 同平台：通过。
	if err := assertPackageMatchesRemote(dir, hostOS, hostArch); err != nil {
		t.Fatalf("same platform must pass, got %v", err)
	}

	// 不同平台：拒绝，并且说清「要在目标平台构建」。
	foreignOS, foreignArch := "linux", "amd64"
	if hostOS == "linux" {
		foreignOS, foreignArch = "darwin", "amd64"
	}
	err = assertPackageMatchesRemote(dir, foreignOS, foreignArch)
	if err == nil {
		t.Fatal("a foreign-platform package must be rejected")
	}
	for _, want := range []string{"产物平台与远端不符", "构建", hostLabel, foreignOS + "/" + foreignArch} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should contain %q, got %v", want, err)
		}
	}

	// 纯脚本包（没有二进制）不参与平台判定。
	scriptsOnly := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scriptsOnly, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scriptsOnly, "scripts", "restart.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := assertPackageMatchesRemote(scriptsOnly, foreignOS, foreignArch); err != nil {
		t.Fatalf("a script-only package has no platform constraint, got %v", err)
	}
}

func TestMachineTargetRestartOverride(t *testing.T) {
	targets, err := ParseMachineTargets(
		"43.162.117.240=ssh agent-oversea /home/ubuntu/runtime restart=sudo sed -i s/APP_VERSION=.*/APP_VERSION={version}/ /etc/systemd/system/autonomyd.service.d/version.conf && sudo systemctl restart autonomyd")
	if err != nil {
		t.Fatalf("ParseMachineTargets: %v", err)
	}
	target := targets["43.162.117.240"]
	if !target.Remote() || target.SSHHost != "agent-oversea" || target.RuntimeHome != "/home/ubuntu/runtime" {
		t.Fatalf("target = %+v", target)
	}
	if !strings.Contains(target.RestartCmd, "systemctl restart autonomyd") {
		t.Fatalf("restart override lost: %q", target.RestartCmd)
	}
	got := target.ExpandRestartCmd("autonomy", "66bf7a82", "/Users/gaolei/runtime/autonomy", "/home/ubuntu/runtime/autonomy", "43.162.117.240")
	for _, want := range []string{"APP_VERSION=66bf7a82", "systemctl restart autonomyd", "{version}"} {
		if want == "{version}" {
			if strings.Contains(got, "{version}") {
				t.Fatalf("placeholder must be expanded, got %q", got)
			}
			continue
		}
		if !strings.Contains(got, want) {
			t.Fatalf("expanded restart cmd should contain %q, got %q", want, got)
		}
	}
}
