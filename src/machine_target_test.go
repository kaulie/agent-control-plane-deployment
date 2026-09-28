package main

import "testing"

// 部署通道（DEPLOY_MACHINE_TARGETS）：机器能不能被选中取决于有没有它。

func TestParseMachineTargets(t *testing.T) {
	targets, err := ParseMachineTargets(
		"local=local; 43.162.117.240=ssh ubuntu@43.162.117.240 /home/ubuntu/runtime;\n" +
			"gpu-2 = ssh 10.0.0.8:2222 /srv/runtime")
	if err != nil {
		t.Fatalf("ParseMachineTargets: %v", err)
	}
	if len(targets) != 3 {
		t.Fatalf("targets = %v, want 3 entries", targets)
	}
	if got := targets["local"]; got.Remote() || got.ID != "local" {
		t.Fatalf("local target = %+v, want a local channel", got)
	}
	remote := targets["43.162.117.240"]
	if !remote.Remote() || remote.SSHUser != "ubuntu" || remote.SSHHost != "43.162.117.240" || remote.SSHPort != 0 {
		t.Fatalf("remote target = %+v", remote)
	}
	if remote.SSHDest() != "ubuntu@43.162.117.240" {
		t.Fatalf("SSHDest = %q", remote.SSHDest())
	}
	// 服务目录按约定 = <remote-home>/<serviceId>。
	if got := remote.RuntimeDirFor("autonomy"); got != "/home/ubuntu/runtime/autonomy" {
		t.Fatalf("RuntimeDirFor = %q", got)
	}
	withPort := targets["gpu-2"]
	if withPort.SSHPort != 2222 || withPort.SSHUser != "" || withPort.RuntimeDirFor("web-cursor") != "/srv/runtime/web-cursor" {
		t.Fatalf("gpu-2 target = %+v", withPort)
	}

	// 本机永远有通道，哪怕配置里一条都没写。
	empty, err := ParseMachineTargets("")
	if err != nil || len(empty) != 1 || !empty["local"].Remote() == false {
		t.Fatalf("empty config must still provide the local channel, got %v err=%v", empty, err)
	}
}

func TestParseMachineTargetsRejectsBadSpecs(t *testing.T) {
	for _, raw := range []string{
		"no-equals-sign",
		"gpu-2=ssh ubuntu@10.0.0.8",               // 缺远端 runtime 目录
		"gpu-2=ssh ubuntu@10.0.0.8 relative/path", // 不是绝对路径
		"gpu-2=ssh ubuntu@10.0.0.8:99999 /srv/r",  // 端口非法
		"gpu-2=ssh user@ /srv/r",                  // 缺主机名
		"gpu-2=vpn 10.0.0.8 /srv/r",               // 不认识的目标类型
	} {
		if _, err := ParseMachineTargets(raw); err == nil {
			t.Fatalf("ParseMachineTargets(%q) should fail", raw)
		}
	}
}
