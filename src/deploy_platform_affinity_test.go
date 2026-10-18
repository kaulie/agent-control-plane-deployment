package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 多台控制面共用一个队列时，「local」在每台机器上都指向自己。
// 本机平台的制品 tag 不带后缀，执行者不能把这种 tag 当成自己的平台。

func foreignBuildPlatform() BuildPlatform {
	local := LocalBuildPlatform()
	if local.OS == "linux" {
		return BuildPlatform{OS: "darwin", Arch: "amd64"}
	}
	return BuildPlatform{OS: "linux", Arch: "amd64"}
}

func TestExplicitArtifactPlatformPrefersRecordedOverTag(t *testing.T) {
	recorded := foreignBuildPlatform()
	job := DeployJob{
		Deployment:       "deployment-abc12345",
		ArtifactPlatform: recorded.String(),
	}
	if got := explicitArtifactPlatform(job); got != recorded {
		t.Fatalf("recorded platform = %v, want %v", got, recorded)
	}
	fromTag := DeployJob{Deployment: "deployment-abc12345-" + recorded.OS + "-" + recorded.Arch}
	if got := explicitArtifactPlatform(fromTag); got != recorded {
		t.Fatalf("tag platform = %v, want %v", got, recorded)
	}
	if got := explicitArtifactPlatform(DeployJob{Deployment: "deployment-abc12345"}); !got.IsZero() {
		t.Fatalf("bare tag without a recorded platform = %v, want zero", got)
	}
}

func TestLocalDeployForThisControlPlane(t *testing.T) {
	localTarget := MachineTarget{ID: "local", Kind: "local"}
	remoteTarget := MachineTarget{ID: "10.0.0.7", Kind: "ssh", SSHHost: "10.0.0.7"}
	foreign := foreignBuildPlatform()

	own := DeployJob{Deployment: "deployment-abc12345", ArtifactPlatform: LocalBuildPlatform().String(), TargetMachine: "local"}
	if !localDeployForThisControlPlane(own, localTarget) {
		t.Fatal("a local deploy of this control plane's platform must run here")
	}
	other := DeployJob{Deployment: "deployment-abc12345", ArtifactPlatform: foreign.String(), TargetMachine: "local"}
	if localDeployForThisControlPlane(other, localTarget) {
		t.Fatal("a local deploy built for another platform must not run on this control plane")
	}
	// 远端部署不看「是不是本控制面的 local」。
	if !localDeployForThisControlPlane(other, remoteTarget) {
		t.Fatal("a remote deploy can be pushed from this control plane")
	}
	// 老任务没记平台：先领走，下载后再看二进制。
	legacy := DeployJob{Deployment: "deployment-abc12345", TargetMachine: "local"}
	if !localDeployForThisControlPlane(legacy, localTarget) {
		t.Fatal("a legacy bare tag has no platform yet and stays claimable")
	}
}

func TestCreateDeployForPlatformRoundTrip(t *testing.T) {
	store := newTestStore(t)
	job, err := store.CreateDeployForPlatform("deploy-plat", "autonomy", "deployment-abc12345",
		Identity{}, "queued", "Linux/amd64", "local")
	if err != nil {
		t.Fatalf("CreateDeployForPlatform: %v", err)
	}
	want, err := ParseBuildPlatform("Linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	if job.ArtifactPlatform != want.String() {
		t.Fatalf("create result platform = %q, want %s", job.ArtifactPlatform, want)
	}
	got, err := store.GetDeploy("deploy-plat")
	if err != nil || got == nil {
		t.Fatalf("GetDeploy: %v %v", got, err)
	}
	if got.ArtifactPlatform != want.String() || got.TargetMachine != "local" {
		t.Fatalf("stored = platform %q machine %q, want %s local", got.ArtifactPlatform, got.TargetMachine, want)
	}
}

func TestDeployWorkerLeavesForeignLocalDeployQueued(t *testing.T) {
	store := newDeployWorkerTestStore(t)
	mustService(t, store, "autonomy", "/tmp/rt-autonomy-affinity")
	mustService(t, store, "knowledge-base", "/tmp/rt-kb-affinity")
	foreign := foreignBuildPlatform()
	if _, err := store.CreateDeployForPlatform("pipeline-foreign", "autonomy", "deployment-foreign1",
		Identity{}, "queued", foreign.String(), "local"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := store.CreateDeployForPlatform("pipeline-mine", "autonomy", "deployment-mine0001",
		Identity{}, "queued", LocalBuildPlatform().String(), "local"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateDeployForPlatform("pipeline-remote", "knowledge-base", "deployment-remote01",
		Identity{}, "queued", foreign.String(), "10.0.0.7"); err != nil {
		t.Fatal(err)
	}

	targets, err := ParseMachineTargets("10.0.0.7=ssh ubuntu@10.0.0.7 /home/ubuntu/runtime")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{DeployMachineTargets: targets}
	worker := NewDeployWorker(store, cfg, nil, &GracefulDrain{}, NewMachineCatalog(cfg, nil), nil)
	runner := newGatedRunner("pipeline-mine", "pipeline-remote")
	worker.run = runner.run

	worker.tick()
	waitForStarted(t, runner, 2)
	if got := deployState(t, store, "pipeline-foreign"); got != StateQueued {
		t.Fatalf("foreign local deploy state = %s, want queued", got)
	}
	if got := deployState(t, store, "pipeline-mine"); got != StateRunning {
		t.Fatalf("same-platform local deploy state = %s, want running", got)
	}
	if got := deployState(t, store, "pipeline-remote"); got != StateRunning {
		t.Fatalf("remote deploy state = %s, want running", got)
	}
	runner.release("pipeline-mine")
	runner.release("pipeline-remote")
	worker.deployWg.Wait()
}

func TestExecuteDeployRequeuesForeignLocalPackage(t *testing.T) {
	store := newTestStore(t)
	mustService(t, store, "autonomy", t.TempDir())
	foreign := foreignBuildPlatform()
	if _, err := store.CreateDeployForPlatform("pipeline-abfb4b15", "autonomy", "deployment-85373c9b",
		Identity{}, "queued", foreign.String(), "local"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimQueued("pipeline-abfb4b15"); err != nil {
		t.Fatal(err)
	}
	targets, err := ParseMachineTargets("")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{DeployMachineTargets: targets, ReleaseMaxSec: 30}
	// storage 为 nil：交回队列必须发生在下载之前，否则这里会空指针。
	executeDeploy(store, cfg, nil, &GracefulDrain{}, NewMachineCatalog(cfg, nil), nil, "pipeline-abfb4b15")

	got, err := store.GetDeploy("pipeline-abfb4b15")
	if err != nil || got == nil {
		t.Fatalf("GetDeploy: %v %v", got, err)
	}
	if got.State != StateQueued {
		t.Fatalf("state = %s, want queued (foreign local deploy must not fail or install)", got.State)
	}
	if got.ArtifactPlatform != foreign.String() {
		t.Fatalf("artifact platform = %q, want %s", got.ArtifactPlatform, foreign)
	}
	events, err := store.ListDeployEvents("pipeline-abfb4b15")
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, e := range events {
		joined += e.Message + "\n"
	}
	for _, part := range []string{"交回队列", "未下载", "未推送"} {
		if !strings.Contains(joined, part) {
			t.Fatalf("timeline missing %q: %s", part, joined)
		}
	}
}

func TestLocalBinaryGuardRequeuesForeignBinary(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.CreateDeploy("deploy-bin", "autonomy", "deployment-abc12345", Identity{}, "queued", "local"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimQueued("deploy-bin"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Skip(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "autonomyd"), data, 0o755); err != nil {
		t.Fatal(err)
	}
	job, _ := store.GetDeploy("deploy-bin")
	requeued, err := localBinaryGuard(store, *job, dir)
	if err != nil {
		t.Fatalf("matching binary: %v", err)
	}
	if requeued {
		t.Fatal("a binary built for this control plane must not be requeued")
	}

	// 脚本包没有平台约束。
	scripts := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scripts, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scripts, "scripts", "start.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	requeued, err = localBinaryGuard(store, *job, scripts)
	if err != nil || requeued {
		t.Fatalf("script package requeued=%v err=%v, want pass", requeued, err)
	}
}
