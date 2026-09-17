package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 旧契约（注册中心接入前本机自建的）历史迁移：把 web-cursor 的流水线 / 部署 /
// 制品索引挂到 agent-control-plane 上，然后删掉旧契约。
func TestMoveServiceHistoryMovesEverythingAndDeletesSource(t *testing.T) {
	store := newCatalogTestStore(t)
	for _, id := range []string{"web-cursor", "agent-control-plane"} {
		if _, err := store.UpsertService(localConfig(id, "")); err != nil {
			t.Fatalf("UpsertService(%s): %v", id, err)
		}
	}
	seedFinishedPipeline(t, store, "pipeline-1", "web-cursor")
	seedFinishedPipeline(t, store, "pipeline-2", "web-cursor")
	seedFinishedPipeline(t, store, "pipeline-keep", "agent-control-plane")
	seedFinishedDeploy(t, store, "deploy-1", "web-cursor")
	seedFinishedDeploy(t, store, "deploy-2", "web-cursor")
	seedArtifact(t, store, "web-cursor", "deployment-aaa")
	seedArtifact(t, store, "web-cursor", "deployment-bbb")

	before, err := store.CountServiceHistory("web-cursor")
	if err != nil {
		t.Fatalf("CountServiceHistory: %v", err)
	}
	if before.Pipelines != 2 || before.Deploys != 2 || before.Artifacts != 2 {
		t.Fatalf("seed counts = %+v, want 2/2/2", before)
	}

	move, err := store.MoveServiceHistory("web-cursor", "agent-control-plane", true)
	if err != nil {
		t.Fatalf("MoveServiceHistory: %v", err)
	}
	if move.Pipelines != 2 || move.Deploys != 2 || move.Artifacts != 2 || move.ArtifactsSkipped != 0 {
		t.Fatalf("move = %+v, want 2/2/2/0", move)
	}
	if !move.DeletedContract {
		t.Fatal("deleteSourceContract=true must report the source contract as deleted")
	}

	if svc, _ := store.GetService("web-cursor"); svc != nil {
		t.Fatalf("old contract must be gone, got %+v", svc)
	}
	if left, _ := store.CountServiceHistory("web-cursor"); left.HasHistory() {
		t.Fatalf("old service must keep no history, got %+v", left)
	}
	// 目标：自己的 1 条流水线 + 迁过来的 2 条。
	got, err := store.CountServiceHistory("agent-control-plane")
	if err != nil {
		t.Fatalf("CountServiceHistory: %v", err)
	}
	if got.Pipelines != 3 || got.Deploys != 2 || got.Artifacts != 2 {
		t.Fatalf("target counts = %+v, want 3 pipelines / 2 deploys / 2 artifacts", got)
	}
	// 历史列表按 serviceId 过滤（面板的筛选走的就是这条查询）。
	deploys, total, err := store.ListDeploysFiltered(ListFilter{ServiceID: "agent-control-plane", Page: 1, PageSize: 50})
	if err != nil {
		t.Fatalf("ListDeploysFiltered: %v", err)
	}
	if total != 2 || len(deploys) != 2 {
		t.Fatalf("moved deploys are not visible under the target service: total=%d len=%d", total, len(deploys))
	}
	// 制品索引也换了服务：老 tag 现在挂在目标服务下（下载走行里的 accessPath）。
	if art, _ := store.GetArtifact("agent-control-plane", "deployment-aaa"); art == nil {
		t.Fatal("artifact deployment-aaa must be reachable under the target service")
	}
	if art, _ := store.GetArtifact("web-cursor", "deployment-aaa"); art != nil {
		t.Fatalf("artifact must not stay under the old service: %+v", art)
	}
}

// 有在途任务时拒绝迁移 —— 一条都不许改（避免把正在跑的任务搬到别的服务名下）。
func TestMoveServiceHistoryRefusesInflightJobs(t *testing.T) {
	store := newCatalogTestStore(t)
	for _, id := range []string{"web-cursor", "agent-control-plane"} {
		if _, err := store.UpsertService(localConfig(id, "")); err != nil {
			t.Fatalf("UpsertService(%s): %v", id, err)
		}
	}
	// queued 的部署任务 + packaging 的流水线。
	if _, err := store.CreateDeploy("deploy-running", "web-cursor", "deployment-aaa", Identity{}, "queued"); err != nil {
		t.Fatalf("CreateDeploy: %v", err)
	}
	if _, err := store.CreatePipeline("pipeline-packaging", "web-cursor", "main", Identity{}, "queued"); err != nil {
		t.Fatalf("CreatePipeline: %v", err)
	}
	if _, err := store.ClaimNextPipeline(); err != nil {
		t.Fatalf("ClaimNextPipeline: %v", err)
	}
	seedFinishedDeploy(t, store, "deploy-done", "web-cursor")

	_, err := store.MoveServiceHistory("web-cursor", "agent-control-plane", true)
	inflight, ok := err.(*ErrServiceInflight)
	if !ok {
		t.Fatalf("err = %v (%T), want *ErrServiceInflight", err, err)
	}
	if inflight.Counts.InflightPipelines != 1 || inflight.Counts.InflightDeploys != 1 {
		t.Fatalf("inflight counts = %+v, want 1 pipeline / 1 deploy", inflight.Counts)
	}
	if !strings.Contains(inflight.Error(), "在途任务") {
		t.Fatalf("error should explain the in-flight block: %v", inflight)
	}

	// 什么都没动：源契约还在、两个 id 的历史都没变。
	if svc, _ := store.GetService("web-cursor"); svc == nil {
		t.Fatal("source contract must survive a refused move")
	}
	got, _ := store.CountServiceHistory("web-cursor")
	if got.Pipelines != 1 || got.Deploys != 2 {
		t.Fatalf("source counts changed after a refused move: %+v", got)
	}
	target, _ := store.CountServiceHistory("agent-control-plane")
	if target.HasHistory() {
		t.Fatalf("target must stay untouched after a refused move: %+v", target)
	}
}

// 目标已经有同一个 tag 的制品索引行：跳过源行（两行是同一个制品），不覆盖、不删除。
func TestMoveServiceHistoryKeepsDuplicateArtifactTags(t *testing.T) {
	store := newCatalogTestStore(t)
	for _, id := range []string{"web-cursor", "agent-control-plane"} {
		if _, err := store.UpsertService(localConfig(id, "")); err != nil {
			t.Fatalf("UpsertService(%s): %v", id, err)
		}
	}
	seedArtifact(t, store, "web-cursor", "deployment-dup")
	seedArtifact(t, store, "web-cursor", "deployment-only-old")
	seedArtifact(t, store, "agent-control-plane", "deployment-dup")

	move, err := store.MoveServiceHistory("web-cursor", "agent-control-plane", false)
	if err != nil {
		t.Fatalf("MoveServiceHistory: %v", err)
	}
	if move.Artifacts != 1 || move.ArtifactsSkipped != 1 {
		t.Fatalf("move = %+v, want 1 moved / 1 skipped", move)
	}
	if move.DeletedContract {
		t.Fatal("deleteSourceContract=false must keep the contract")
	}
	if svc, _ := store.GetService("web-cursor"); svc == nil {
		t.Fatal("source contract must be kept")
	}
	// 目标：重复 tag 只有一行；老服务还剩那一行（未做破坏性删除）。
	arts, err := store.ListArtifacts("agent-control-plane")
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(arts) != 2 {
		t.Fatalf("target artifacts = %d, want 2 (dup + moved)", len(arts))
	}
	old, _ := store.ListArtifacts("web-cursor")
	if len(old) != 1 || old[0].Tag != "deployment-dup" {
		t.Fatalf("duplicate source row should stay put, got %+v", old)
	}
}

func TestMoveServiceHistoryRejectsBadInput(t *testing.T) {
	store := newCatalogTestStore(t)
	if _, err := store.UpsertService(localConfig("web-cursor", "")); err != nil {
		t.Fatalf("UpsertService: %v", err)
	}
	if _, err := store.MoveServiceHistory("web-cursor", "web-cursor", false); err == nil {
		t.Fatal("moving a service onto itself must fail")
	}
	if _, err := store.MoveServiceHistory("", "x", false); err == nil {
		t.Fatal("empty from must fail")
	}
}

// GET /api/services/{id}/history：给面板确认框用的数据（条数 + 可迁移目标）。
func TestHandleServiceHistoryListsCountsAndTargets(t *testing.T) {
	store := newCatalogTestStore(t)
	for _, id := range []string{"web-cursor", "agent-control-plane"} {
		if _, err := store.UpsertService(localConfig(id, "")); err != nil {
			t.Fatalf("UpsertService(%s): %v", id, err)
		}
	}
	seedFinishedPipeline(t, store, "pipeline-1", "web-cursor")
	seedFinishedDeploy(t, store, "deploy-1", "web-cursor")
	seedArtifact(t, store, "web-cursor", "deployment-aaa")

	// event-center 已在注册中心登记但本机没配置 → 不能作为迁移目标。
	srv := fakeServiceRegistry(t, []RegistryService{{Name: "event-center"}, {Name: "web-cursor"}})
	api := &apiServer{store: store, registry: registryClientFor(srv.URL)}

	rec := serviceRequest(t, api, http.MethodGet, "/api/services/web-cursor/history", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET history = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		ServiceID string                 `json:"serviceId"`
		History   ServiceHistoryCounts   `json:"history"`
		Inflight  int                    `json:"inflight"`
		Targets   []ServiceHistoryTarget `json:"targets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ServiceID != "web-cursor" || got.History.Pipelines != 1 || got.History.Deploys != 1 || got.History.Artifacts != 1 {
		t.Fatalf("history = %+v", got)
	}
	if got.Inflight != 0 {
		t.Fatalf("inflight = %d, want 0", got.Inflight)
	}
	if len(got.Targets) != 1 || got.Targets[0].ServiceID != "agent-control-plane" {
		t.Fatalf("targets = %+v, want only the configured agent-control-plane", got.Targets)
	}
	if got.Targets[0].RuntimeDir != "/tmp/agent-control-plane" {
		t.Fatalf("target should carry runtimeDir（面板用来预选）: %+v", got.Targets[0])
	}

	// 未知 serviceId → 404。
	if rec := serviceRequest(t, api, http.MethodGet, "/api/services/nope/history", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("GET history for unknown service = %d, want 404", rec.Code)
	}
}

// POST /api/services/{id}/history/move：迁移 + 删旧契约（面板「清除」走这一条）。
func TestHandleMoveServiceHistoryEndpoint(t *testing.T) {
	store := newCatalogTestStore(t)
	for _, id := range []string{"web-cursor", "agent-control-plane"} {
		if _, err := store.UpsertService(localConfig(id, "")); err != nil {
			t.Fatalf("UpsertService(%s): %v", id, err)
		}
	}
	seedFinishedPipeline(t, store, "pipeline-1", "web-cursor")
	seedFinishedDeploy(t, store, "deploy-1", "web-cursor")
	api := &apiServer{store: store}

	rec := serviceRequest(t, api, http.MethodPost, "/api/services/web-cursor/history/move",
		`{"to":"agent-control-plane","deleteSourceContract":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST move = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		OK      bool               `json:"ok"`
		Move    ServiceHistoryMove `json:"move"`
		Message string             `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.OK || got.Move.Pipelines != 1 || got.Move.Deploys != 1 || !got.Move.DeletedContract {
		t.Fatalf("response = %+v", got)
	}
	if !strings.Contains(got.Message, "agent-control-plane") || !strings.Contains(got.Message, "删除") {
		t.Fatalf("message should say where the history went and that the contract was deleted: %q", got.Message)
	}
	if svc, _ := store.GetService("web-cursor"); svc != nil {
		t.Fatal("source contract must be deleted")
	}

	// 目标没有本机配置 → 400（不能把历史挂到一个没法部署的服务上）。
	rec = serviceRequest(t, api, http.MethodPost, "/api/services/agent-control-plane/history/move", `{"to":"ghost"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "ghost") {
		t.Fatalf("move to an unconfigured target = %d, body=%s", rec.Code, rec.Body.String())
	}
	// 迁给自己 → 400。
	rec = serviceRequest(t, api, http.MethodPost, "/api/services/agent-control-plane/history/move",
		`{"to":"agent-control-plane"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("move onto itself = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	// 缺 to → 400。
	rec = serviceRequest(t, api, http.MethodPost, "/api/services/agent-control-plane/history/move", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("move without to = %d, want 400", rec.Code)
	}
}

// DELETE：有在途任务 → 409；只有已完成的历史 → 允许（配置删掉，历史留在原地）。
func TestDeleteServiceGuardsInflightAndReportsLeftoverHistory(t *testing.T) {
	store := newCatalogTestStore(t)
	for _, id := range []string{"web-cursor", "old-svc"} {
		if _, err := store.UpsertService(localConfig(id, "")); err != nil {
			t.Fatalf("UpsertService(%s): %v", id, err)
		}
	}
	if _, err := store.CreateDeploy("deploy-queued", "web-cursor", "deployment-aaa", Identity{}, "queued"); err != nil {
		t.Fatalf("CreateDeploy: %v", err)
	}
	seedFinishedPipeline(t, store, "pipeline-1", "old-svc")
	api := &apiServer{store: store}

	rec := serviceRequest(t, api, http.MethodDelete, "/api/services/web-cursor", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("DELETE with a queued deploy = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "在途任务") || !strings.Contains(rec.Body.String(), "history/move") {
		t.Fatalf("409 should explain the block and point at the move endpoint: %s", rec.Body.String())
	}
	if svc, _ := store.GetService("web-cursor"); svc == nil {
		t.Fatal("a refused delete must keep the contract")
	}

	rec = serviceRequest(t, api, http.MethodDelete, "/api/services/old-svc", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE old-svc = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "pipelines") {
		t.Fatalf("DELETE should report the history it left behind: %s", rec.Body.String())
	}
	if svc, _ := store.GetService("old-svc"); svc != nil {
		t.Fatal("contract should be deleted")
	}
	left, _ := store.CountServiceHistory("old-svc")
	if left.Pipelines != 1 {
		t.Fatalf("finished history must stay (it is only orphaned): %+v", left)
	}
}

func seedFinishedPipeline(t *testing.T, store *Store, requestID, serviceID string) {
	t.Helper()
	if _, err := store.CreatePipeline(requestID, serviceID, "main", Identity{}, "queued"); err != nil {
		t.Fatalf("CreatePipeline(%s): %v", requestID, err)
	}
	if err := store.UpdatePipeline(requestID, PipelineJob{State: PipelineSucceeded, Message: "pipeline succeeded"}); err != nil {
		t.Fatalf("UpdatePipeline(%s): %v", requestID, err)
	}
}

func seedFinishedDeploy(t *testing.T, store *Store, requestID, serviceID string) {
	t.Helper()
	if _, err := store.CreateDeploy(requestID, serviceID, "deployment-"+requestID, Identity{}, "queued"); err != nil {
		t.Fatalf("CreateDeploy(%s): %v", requestID, err)
	}
	if _, err := store.FinishDeploy(requestID, FinishPatch{State: StateSucceeded, Message: "deployed"}); err != nil {
		t.Fatalf("FinishDeploy(%s): %v", requestID, err)
	}
}

func seedArtifact(t *testing.T, store *Store, serviceID, tag string) {
	t.Helper()
	if err := store.RecordArtifact(Artifact{
		ServiceID: serviceID, Tag: tag, Version: tag,
		GitRepoURL: "https://github.com/kaulie/" + serviceID, RepoSlug: "kaulie/" + serviceID,
		AssetURL: "https://packages.example.com/" + serviceID + "/" + tag, Storage: "aliyun",
	}); err != nil {
		t.Fatalf("RecordArtifact(%s/%s): %v", serviceID, tag, err)
	}
}

// serviceRequest 走真实的 routes()，这样路径参数、方法与状态码都和线上一致。
func serviceRequest(t *testing.T, api *apiServer, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	api.routes().ServeHTTP(rec, req)
	return rec
}
