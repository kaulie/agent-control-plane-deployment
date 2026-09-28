package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// 「部署机器」选项：解析 / 归一化 / 校验。

func TestParseDeployMachines(t *testing.T) {
	got := parseDeployMachines("local, mac2 ,local,,build;x\nweb-1")
	want := []string{"local", "mac2", "build", "x", "web-1"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("parseDeployMachines = %v, want %v (trim/dedupe/keep order)", got, want)
	}
	if parseDeployMachines("   ") != nil {
		t.Fatalf("blank config must yield nil (caller falls back to the single machine)")
	}
}

func TestConfigDeployMachineResolution(t *testing.T) {
	// 都不配置 → 单机缺省 "local"，行为与「不选机器」一致。
	var zero Config
	if ids := zero.DeployMachineIDs(); len(ids) != 1 || ids[0] != defaultDeployMachine {
		t.Fatalf("default machines = %v, want [%s]", ids, defaultDeployMachine)
	}
	if got := zero.DefaultDeployMachineID(); got != defaultDeployMachine {
		t.Fatalf("default machine = %q, want %q", got, defaultDeployMachine)
	}
	if got, err := zero.ValidateDeployMachine(""); err != nil || got != defaultDeployMachine {
		t.Fatalf("empty selection must resolve to the default, got %q err=%v", got, err)
	}
	if _, err := zero.ValidateDeployMachine("gpu-2"); err == nil {
		t.Fatal("an unknown machine must be rejected")
	}

	// 显式配置：默认机器取 DefaultDeployMachine；列表成员放行，其余 400。
	c := Config{DeployMachines: []string{"local", "gpu-2"}, DefaultDeployMachine: "gpu-2"}
	if got := c.DefaultDeployMachineID(); got != "gpu-2" {
		t.Fatalf("default = %q, want gpu-2", got)
	}
	if got, err := c.ValidateDeployMachine("  local "); err != nil || got != "local" {
		t.Fatalf("known machine must be trimmed and accepted, got %q err=%v", got, err)
	}
	if _, err := c.ValidateDeployMachine("nope"); err == nil || !strings.Contains(err.Error(), "allowed") {
		t.Fatalf("unknown machine error should list the allowed ids, got %v", err)
	}
}

// 触发流水线时选择的机器必须落库（打包/部署稍后由 worker 干，选项要跟着走）。
func TestDeployNotifyPersistsTargetMachine(t *testing.T) {
	srv, store := newTestIdentityServer(t, true)
	srv.cfg.DeployMachines = []string{"local", "gpu-2"}
	srv.cfg.DefaultDeployMachine = "local"
	// 机器要被选中，得有部署通道（怎么把部署送到那台机器）。
	srv.cfg.DeployMachineTargets = map[string]MachineTarget{
		"local": {ID: "local", Kind: "local"},
		"gpu-2": {ID: "gpu-2", Kind: "ssh", SSHUser: "ubuntu", SSHHost: "10.0.0.8", RuntimeHome: "/home/ubuntu/runtime"},
	}
	srv.machines = NewMachineCatalog(srv.cfg, nil)
	hdr := map[string]string{"identity_role": "user", "identity_id": "user_001"}

	// 显式选择 → 落库并在响应里回显。
	rec := postJSON(t, srv, "/api/deploy-notify",
		`{"serviceId":"web-cursor","targetMachine":"gpu-2"}`, hdr)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		RequestID     string `json:"requestId"`
		TargetMachine string `json:"targetMachine"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.TargetMachine != "gpu-2" {
		t.Fatalf("response must echo targetMachine=gpu-2, got %s", rec.Body.String())
	}
	job, err := store.GetPipeline(resp.RequestID)
	if err != nil || job == nil {
		t.Fatalf("GetPipeline(%s): job=%v err=%v", resp.RequestID, job, err)
	}
	if job.TargetMachine != "gpu-2" {
		t.Fatalf("targetMachine must be persisted on the pipeline, got %q", job.TargetMachine)
	}
	events, _ := store.ListPipelineEvents(resp.RequestID)
	joined := ""
	for _, e := range events {
		joined += e.Message + "\n"
	}
	if !strings.Contains(joined, "部署机器=gpu-2") {
		t.Fatalf("the timeline should name the deploy machine, got:\n%s", joined)
	}

	// 不选 → 默认机器。
	rec = postJSON(t, srv, "/api/deploy-notify", `{"serviceId":"web-cursor"}`, hdr)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	job, _ = store.GetPipeline(resp.RequestID)
	if job == nil || job.TargetMachine != "local" {
		t.Fatalf("no selection must fall back to the default machine, got %+v", job)
	}

	// 未知机器 → 400（在创建流水线之前拒绝）。
	rec = postJSON(t, srv, "/api/deploy-notify",
		`{"serviceId":"web-cursor","targetMachine":"ghost"}`, hdr)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown machine must be rejected with 400, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMetaReportsDeployMachines(t *testing.T) {
	srv, _ := newTestIdentityServer(t, true)
	srv.cfg.DeployMachines = []string{"local", "gpu-2"}
	srv.cfg.DefaultDeployMachine = "gpu-2"
	srv.cfg.DeployMachineTargets = map[string]MachineTarget{
		"local": {ID: "local", Kind: "local"},
		"gpu-2": {ID: "gpu-2", Kind: "ssh", SSHUser: "ubuntu", SSHHost: "10.0.0.8", RuntimeHome: "/home/ubuntu/runtime"},
	}
	srv.machines = NewMachineCatalog(srv.cfg, nil)

	var meta map[string]any
	if err := json.Unmarshal(getJSON(t, srv, "/api/meta").Body.Bytes(), &meta); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	ids, _ := meta["deployMachines"].([]any)
	if len(ids) != 2 || ids[0] != "local" || ids[1] != "gpu-2" {
		t.Fatalf("deployMachines = %v, want [local gpu-2]", meta["deployMachines"])
	}
	if meta["defaultDeployMachine"] != "gpu-2" {
		t.Fatalf("defaultDeployMachine = %v, want gpu-2", meta["defaultDeployMachine"])
	}
	targets, _ := meta["deployMachineTargets"].(map[string]any)
	if got := targets["gpu-2"].(map[string]any)["kind"]; got != "ssh" {
		t.Fatalf("deployMachineTargets[gpu-2].kind = %v, want ssh（面板据此标注远端）", got)
	}
	if got := targets["local"].(map[string]any)["kind"]; got != "local" {
		t.Fatalf("deployMachineTargets[local].kind = %v, want local", got)
	}
}

// 老库（pipelines / deploys 都没有 target_machine 列）打开时自动补列，老行读到空串。
func TestDeployMachineColumnMigration(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "old.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`
      CREATE TABLE pipelines (
        request_id TEXT PRIMARY KEY, service_id TEXT NOT NULL, ref TEXT NOT NULL,
        state TEXT NOT NULL, deployment TEXT, deploy_request_id TEXT, version TEXT,
        error TEXT, message TEXT, requested_at TEXT NOT NULL, started_at TEXT, finished_at TEXT,
        triggered_by_role TEXT NOT NULL DEFAULT '', triggered_by_id TEXT NOT NULL DEFAULT ''
      );
      CREATE TABLE deploys (
        request_id TEXT PRIMARY KEY, service_id TEXT NOT NULL, deployment TEXT NOT NULL,
        state TEXT NOT NULL, requested_at TEXT NOT NULL, started_at TEXT, finished_at TEXT,
        version TEXT, error TEXT, message TEXT,
        triggered_by_role TEXT NOT NULL DEFAULT '', triggered_by_id TEXT NOT NULL DEFAULT ''
      );
      INSERT INTO pipelines (request_id, service_id, ref, state, requested_at)
        VALUES ('pipeline-old', 'web-cursor', 'main', 'succeeded', '2026-09-20T00:00:00.000Z');
      INSERT INTO deploys (request_id, service_id, deployment, state, requested_at)
        VALUES ('deploy-old', 'web-cursor', 'deployment-abc', 'succeeded', '2026-09-20T00:00:00.000Z');
    `); err != nil {
		t.Fatalf("seed old schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore (migration): %v", err)
	}
	defer store.Close()

	oldP, err := store.GetPipeline("pipeline-old")
	if err != nil || oldP == nil {
		t.Fatalf("GetPipeline(old): job=%v err=%v", oldP, err)
	}
	if oldP.TargetMachine != "" {
		t.Fatalf("old pipeline rows must default to no machine, got %q", oldP.TargetMachine)
	}
	oldD, err := store.GetDeploy("deploy-old")
	if err != nil || oldD == nil {
		t.Fatalf("GetDeploy(old): job=%v err=%v", oldD, err)
	}
	if oldD.TargetMachine != "" {
		t.Fatalf("old deploy rows must default to no machine, got %q", oldD.TargetMachine)
	}

	// 新记录带上机器，和迁移过来的老行共存。
	if _, err := store.CreatePipeline("pipeline-new", "web-cursor", "main", false, Identity{}, "queued", "gpu-2"); err != nil {
		t.Fatalf("CreatePipeline: %v", err)
	}
	freshP, _ := store.GetPipeline("pipeline-new")
	if freshP == nil || freshP.TargetMachine != "gpu-2" {
		t.Fatalf("targetMachine must round-trip on the pipeline, got %+v", freshP)
	}
	if _, err := store.CreateDeploy("deploy-new", "web-cursor", "deployment-abc", Identity{}, "queued", "gpu-2"); err != nil {
		t.Fatalf("CreateDeploy: %v", err)
	}
	freshD, _ := store.GetDeploy("deploy-new")
	if freshD == nil || freshD.TargetMachine != "gpu-2" {
		t.Fatalf("targetMachine must round-trip on the deploy, got %+v", freshD)
	}
}
