package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsSelfDeploy(t *testing.T) {
	home := t.TempDir()
	cfg := Config{Home: home}
	svc := ServiceContract{RuntimeDir: home}
	if !isSelfDeploy(svc, cfg) {
		t.Fatal("same path should be self deploy")
	}
	svc.RuntimeDir = filepath.Join(home, "other")
	if isSelfDeploy(svc, cfg) {
		t.Fatal("different path should not be self deploy")
	}
}

func TestEnqueueACPUpgrade(t *testing.T) {
	home := t.TempDir()
	cfg := Config{Home: home}
	job := DeployJob{RequestID: "deploy-req-test1", ServiceID: "acp", Deployment: "deployment-abc"}
	svc := ServiceContract{
		ServiceID:  "acp",
		RuntimeDir: home,
		HealthURL:  "http://127.0.0.1:4220/health",
	}
	if err := enqueueACPUpgrade(cfg, job, svc, "abc", "deployment-abc"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "upgrade-requests", "deploy-req-test1.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "deploy-req-test1") {
		t.Fatalf("unexpected request body: %s", raw)
	}
}

func TestSelfDeployRsyncPreservesData(t *testing.T) {
	cmd := selfDeployRsyncCmd("/pkg", "/home")
	for _, needle := range []string{"packages/", "data/", "upgrade-requests/", "upgrader.pid", "go/", ".cache/", "Library/"} {
		if !strings.Contains(cmd, needle) {
			t.Fatalf("rsync cmd missing protect for %s: %s", needle, cmd)
		}
	}
}
