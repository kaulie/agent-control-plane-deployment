package main

import (
	"strings"
	"testing"
)

// 机器的 platform 是注册中心上的字段（实例 metadata.platform），控制面只同步、不自己构造。

func TestInstancePlatformFromRegistryMetadata(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]string
		want string
	}{
		{"platform 字段", map[string]string{"platform": "linux/amd64"}, "linux/amd64"},
		{"platform 大小写与空格", map[string]string{"Platform": " Linux/ARM64 "}, "linux/arm64"},
		{"os+arch 两个字段", map[string]string{"os": "linux", "arch": "arm64"}, "linux/arm64"},
		{"GOOS/GOARCH 写法", map[string]string{"GOOS": "linux", "GOARCH": "amd64"}, "linux/amd64"},
		{"只给 os → 按本机架构补齐", map[string]string{"os": "linux"}, "linux/" + LocalBuildPlatform().Arch},
		{"没登记", nil, ""},
		{"只给了不认识的值", map[string]string{"platform": "windows/amd64"}, ""},
	}
	for _, c := range cases {
		got := InstancePlatform(RegistryInstance{Host: "10.0.0.7", Metadata: c.meta})
		if got.String() != c.want {
			t.Fatalf("%s: InstancePlatform = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRegisteredOSPrefersInstanceField(t *testing.T) {
	linuxOnly := RegistryInstance{Host: "49.234.45.173", OS: "linux"}
	if osName, ok := linuxOnly.RegisteredOS(); !ok || osName != "linux" {
		t.Fatalf("top-level os=linux → %q ok=%v", osName, ok)
	}
	if !InstancePlatform(linuxOnly).IsZero() {
		t.Fatal("os without arch must not become a build platform")
	}
	mac := RegistryInstance{Host: "127.0.0.1", OS: "mac"}
	if osName, ok := mac.RegisteredOS(); !ok || osName != "darwin" {
		t.Fatalf("top-level os=mac → %q ok=%v, want darwin", osName, ok)
	}
	// 老实例没有顶层 os 时，仍认 metadata.platform 里的 OS。
	legacy := RegistryInstance{Host: "10.0.0.7", Metadata: map[string]string{"platform": "linux/amd64"}}
	if osName, ok := legacy.RegisteredOS(); !ok || osName != "linux" {
		t.Fatalf("metadata.platform fallback = %q ok=%v", osName, ok)
	}
	if InstancePlatform(legacy).String() != "linux/amd64" {
		t.Fatalf("build platform = %s, want linux/amd64", InstancePlatform(legacy))
	}
}

func TestCatalogInstanceOSDoesNotInventBuildArch(t *testing.T) {
	reg := fakeRegistry(t, `{
	  "services":[{"namespace":"default","name":"agent-control-plane"}],
	  "instances":[
	    {"namespace":"default","service":"agent-control-plane","host":"127.0.0.1","port":4211,"os":"mac"},
	    {"namespace":"default","service":"agent-control-plane","host":"49.234.45.173","port":4211,"os":"linux"}
	  ]
	}`, true)
	cat := NewMachineCatalog(Config{DeployMachineTargets: map[string]MachineTarget{
		"local": {ID: "local", Kind: "local"},
	}}, reg)
	if osName, ok := cat.InstanceOS(t.Context(), "agent-control-plane", "49.234.45.173"); !ok || osName != "linux" {
		t.Fatalf("InstanceOS(linux host) = %q ok=%v", osName, ok)
	}
	if osName, ok := cat.InstanceOS(t.Context(), "agent-control-plane", "local"); !ok || osName != "darwin" {
		t.Fatalf("InstanceOS(local mac) = %q ok=%v", osName, ok)
	}
	if _, ok := cat.PlatformFor(t.Context(), "49.234.45.173"); ok {
		t.Fatal("os=linux without arch must not be stored as a build platform")
	}
}

func TestMachineFromInstanceUsesMachineAndPlatformFields(t *testing.T) {
	inst := RegistryInstance{
		Service: "autonomy", Host: "43.162.117.240", Port: 4300,
		Metadata: map[string]string{"machine": "agent-oversea", "platform": "linux/amd64"},
	}
	m := MachineFromInstance(inst)
	if m.ID != "agent-oversea" || m.Platform.String() != "linux/amd64" {
		t.Fatalf("machine = %+v, want id=agent-oversea platform=linux/amd64", m)
	}
	// 本机实例仍归并成 local（平台由控制面自己知道）
	local := MachineFromInstance(RegistryInstance{Host: "127.0.0.1", Port: 4300})
	if local.ID != defaultDeployMachine {
		t.Fatalf("loopback instance machine = %q, want %q", local.ID, defaultDeployMachine)
	}
}

// 目录从注册中心同步「机器 → 平台」，/api/meta 把它带出来。
func TestCatalogSyncsMachinePlatformFromRegistry(t *testing.T) {
	reg := fakeRegistry(t, `{"services": [], "instances": [
		{"namespace":"default","service":"autonomy","host":"10.0.0.7","port":4300,"metadata":{"platform":"linux/arm64"}},
		{"namespace":"default","service":"web-cursor","host":"10.0.0.9","port":4211}
	]}`, true)
	cfg := Config{DeployMachineTargets: map[string]MachineTarget{
		"local":    {ID: "local", Kind: "local"},
		"10.0.0.7": {ID: "10.0.0.7", Kind: "ssh", SSHHost: "10.0.0.7", RuntimeHome: "/srv"},
		"10.0.0.9": {ID: "10.0.0.9", Kind: "ssh", SSHHost: "10.0.0.9", RuntimeHome: "/srv"},
	}}
	cat := NewMachineCatalog(cfg, reg)
	if p, ok := cat.PlatformFor(t.Context(), "10.0.0.7"); !ok || p.String() != "linux/arm64" {
		t.Fatalf("PlatformFor(10.0.0.7) = (%v, %v), want linux/arm64", p, ok)
	}
	if _, ok := cat.PlatformFor(t.Context(), "10.0.0.9"); ok {
		t.Fatal("a machine without a registered platform must not report one")
	}

	srv, _ := newTestIdentityServer(t, true)
	srv.registry = reg
	srv.machines = NewMachineCatalog(cfg, reg)
	body := getJSON(t, srv, "/api/meta").Body.String()
	if !strings.Contains(body, "linux/arm64") {
		t.Fatalf("/api/meta must expose the machine's registered platform, got %s", body)
	}
}

// 通道里再写 platform= 会被明确拒绝（平台只在注册中心登记）。
func TestMachineTargetsRejectPlatformInChannel(t *testing.T) {
	_, err := ParseMachineTargets("43.162.117.240=ssh agent-oversea /home/ubuntu/runtime platform=linux/amd64")
	if err == nil {
		t.Fatal("platform= in the deploy channel must be rejected")
	}
	for _, want := range []string{"service_registry", "metadata.platform"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should point at the registry, got %v", err)
		}
	}
}
