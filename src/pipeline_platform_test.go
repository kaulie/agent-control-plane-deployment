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
			"local": {ID: "local", Kind: "local"},
			"explicit": {ID: "explicit", Kind: "ssh", SSHHost: "h1", RuntimeHome: "/srv",
				Platform: "linux/arm64"},
			"probed": {ID: "probed", Kind: "ssh", SSHHost: "h2", RuntimeHome: "/srv"},
		},
	}
	fake := &fakeRemote{remoteOS: "Linux", remoteArch: "x86_64"}
	w := &PipelineWorker{store: store, cfg: cfg,
		machines:  NewMachineCatalog(cfg, nil),
		platforms: newPlatformResolver(fake)}

	if got := w.platformFor(&PipelineJob{RequestID: "p1", TargetMachine: "local"}); got != LocalBuildPlatform() {
		t.Fatalf("local machine = %v, want the control plane platform", got)
	}
	if got := w.platformFor(&PipelineJob{RequestID: "p2", TargetMachine: "explicit"}); got.String() != "linux/arm64" {
		t.Fatalf("declared platform = %v, want linux/arm64", got)
	}
	if got := w.platformFor(&PipelineJob{RequestID: "p3", TargetMachine: "probed"}); got.String() != "linux/amd64" {
		t.Fatalf("probed platform = %v, want linux/amd64", got)
	}
	runs, _, _ := fake.calls()
	joined := strings.Join(runs, "\n")
	if !strings.Contains(joined, "uname -s") {
		t.Fatalf("a remote machine without a declared platform must be probed, got:\n%s", joined)
	}
}
