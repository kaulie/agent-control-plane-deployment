package main

import (
	"strings"
	"testing"
)

// 打包平台解析：通道显式声明优先；本机机器 = 控制面平台；远端没声明则 ssh 探测。
func TestPipelinePlatformFor(t *testing.T) {
	_, store := newTestIdentityServer(t, false)
	cfg := Config{
		DeployMachineTargets: map[string]MachineTarget{
			"local":    {ID: "local", Kind: "local"},
			"probed":   {ID: "probed", Kind: "ssh", SSHHost: "h2", RuntimeHome: "/srv"},
			"from-reg": {ID: "from-reg", Kind: "ssh", SSHHost: "h3", RuntimeHome: "/srv"},
		},
	}
	fake := &fakeRemote{remoteOS: "Linux", remoteArch: "x86_64"}
	// 注册中心里 from-reg 这台机器登记了平台（机器的字段，控制面同步它）。
	reg := fakeRegistry(t, `{"services": [], "instances": [
		{"namespace":"default","service":"autonomy","host":"h3","port":4300,"metadata":{"machine":"from-reg","platform":"linux/arm64"}}
	]}`, true)
	w := &PipelineWorker{store: store, cfg: cfg,
		machines:  NewMachineCatalog(cfg, reg),
		platforms: newPlatformResolver(fake)}

	if got := w.platformFor(&PipelineJob{RequestID: "p1", TargetMachine: "local"}); got != LocalBuildPlatform() {
		t.Fatalf("local machine = %v, want the control plane platform", got)
	}
	// ① 注册中心登记的机器字段优先（不必 ssh 探测）
	if got := w.platformFor(&PipelineJob{RequestID: "p2", TargetMachine: "from-reg"}); got.String() != "linux/arm64" {
		t.Fatalf("registry-registered platform = %v, want linux/arm64", got)
	}
	runs, _, _ := fake.calls()
	if strings.Contains(strings.Join(runs, "\n"), "uname -s") {
		t.Fatalf("a machine whose platform is registered must not be probed, got:\n%s", strings.Join(runs, "\n"))
	}
	// ② 注册中心没登记 → 探测兜底
	if got := w.platformFor(&PipelineJob{RequestID: "p3", TargetMachine: "probed"}); got.String() != "linux/amd64" {
		t.Fatalf("probed platform = %v, want linux/amd64", got)
	}
	runs, _, _ = fake.calls()
	if !strings.Contains(strings.Join(runs, "\n"), "uname -s") {
		t.Fatal("a machine without a registered platform must be probed")
	}
}
