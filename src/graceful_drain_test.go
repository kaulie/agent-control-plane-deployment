package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func newDrainTestServer(t *testing.T) (*Store, *GracefulDrain, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	store, err := NewStore(filepath.Join(dir, "test.sqlite"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	drain := &GracefulDrain{}
	api := &apiServer{store: store, cfg: Config{}, drain: drain}
	srv := httptest.NewServer(api.routes())
	return store, drain, srv
}

func drainGet(t *testing.T, url string) map[string]any {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer res.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(res.Body).Decode(&m)
	return m
}

func drainPost(t *testing.T, url string, body any) (map[string]any, int) {
	t.Helper()
	b, _ := json.Marshal(body)
	res, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer res.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(res.Body).Decode(&m)
	return m, res.StatusCode
}

func TestRestartNotifyPoll(t *testing.T) {
	store, drain, srv := newDrainTestServer(t)
	defer store.Close()
	defer srv.Close()

	// Before notify: not draining, canRestart=false.
	poll := drainGet(t, srv.URL+"/restart/poll")
	if poll["draining"] != false || poll["canRestart"] != false {
		t.Fatalf("pre-notify poll = %v", poll)
	}

	// Notify.
	nb, code := drainPost(t, srv.URL+"/restart/notify", map[string]any{
		"serviceId":  "agent-control-plane-deployment",
		"requestId":  "pipeline-self1",
		"deployment": "deployment-abc",
		"message":    "x",
	})
	if code != http.StatusAccepted || nb["draining"] != true {
		t.Fatalf("notify resp code=%d body=%v", code, nb)
	}
	if !drain.IsDraining() || drain.RestartingID() != "pipeline-self1" {
		t.Fatalf("drain state: draining=%v id=%q", drain.IsDraining(), drain.RestartingID())
	}

	// No other in-flight work → can restart.
	poll = drainGet(t, srv.URL+"/restart/poll")
	if poll["canRestart"] != true {
		t.Fatalf("expected canRestart=true, got %v", poll)
	}

	// Another running deploy (not the restarting one) blocks restart.
	_, _ = store.CreateDeploy("deploy-other", "web-cursor", "deployment-xyz", Identity{}, "running")
	if _, err := store.ClaimNextQueued(); err != nil {
		t.Fatalf("ClaimNextQueued: %v", err)
	}
	poll = drainGet(t, srv.URL+"/restart/poll")
	if poll["canRestart"] != false {
		t.Fatalf("expected canRestart=false with other in-flight, got %v", poll)
	}
	if n, _ := poll["inflightDeploys"].(float64); n != 1 {
		t.Fatalf("expected 1 inflight deploy, got %v", poll["inflightDeploys"])
	}

	// The restarting job itself is excluded.
	_, _ = store.CreateDeploy("pipeline-self1", "agent-control-plane-deployment", "deployment-abc", Identity{}, "running")
	if _, err := store.ClaimNextQueued(); err != nil {
		t.Fatalf("ClaimNextQueued self: %v", err)
	}
	poll = drainGet(t, srv.URL+"/restart/poll")
	if poll["canRestart"] != false {
		t.Fatalf("self deploy must be excluded; got %v", poll)
	}
	if n, _ := poll["inflightDeploys"].(float64); n != 1 {
		t.Fatalf("self deploy should not count, got %v", poll["inflightDeploys"])
	}

	// Finish the other deploy → can restart again.
	_, _ = store.FinishDeploy("deploy-other", FinishPatch{State: StateSucceeded, Version: "xyz"})
	poll = drainGet(t, srv.URL+"/restart/poll")
	if poll["canRestart"] != true {
		t.Fatalf("expected canRestart=true after other finished, got %v", poll)
	}

	// Clear → not draining.
	drain.Clear()
	poll = drainGet(t, srv.URL+"/restart/poll")
	if poll["draining"] != false {
		t.Fatalf("expected draining=false after clear, got %v", poll)
	}
}

// A pipeline can be claimed a moment before a self-deploy starts draining:
// it finishes packaging and enqueues its deploy, but the deploy worker refuses
// to claim jobs while draining, so that deploy stays queued. Counting such a
// pipeline as in-flight work deadlocks the restart (the poll never becomes
// ready, and the queued deploy only starts once the process restarts). The
// restart window must ignore it — while still honouring pipelines that are
// really working (packaging, or deploying with the deploy already running).
func TestRestartPollIgnoresPipelinesWaitingForTheirDeploy(t *testing.T) {
	store, _, srv := newDrainTestServer(t)
	defer store.Close()
	defer srv.Close()

	_, _ = drainPost(t, srv.URL+"/restart/notify", map[string]any{
		"serviceId": "agent-control-plane-deployment",
		"requestId": "pipeline-self1",
		"message":   "x",
	})

	// Pipeline claimed just before the drain: packaged, deploy enqueued (queued).
	_, _ = store.CreatePipeline("pipeline-3318ac5e", "agent-benchmark-tool", "main", Identity{}, "queued")
	if err := store.UpdatePipeline("pipeline-3318ac5e", PipelineJob{
		State:           PipelineDeploying,
		Deployment:      "deployment-cabb1e98",
		DeployRequestID: "pipeline-3318ac5e",
		Message:         "deploy queued; waiting for graceful restart window then apply",
	}); err != nil {
		t.Fatalf("UpdatePipeline: %v", err)
	}
	_, _ = store.CreateDeploy("pipeline-3318ac5e", "agent-benchmark-tool", "deployment-cabb1e98",
		Identity{}, "queued after package (graceful notify+poll before restart)")

	poll := drainGet(t, srv.URL+"/restart/poll")
	if poll["canRestart"] != true {
		t.Fatalf("a queued deploy must not block the restart window, got %v", poll)
	}
	if n, _ := poll["inflightPipes"].(float64); n != 0 {
		t.Fatalf("a queued deploy must not count as an in-flight pipeline, got %v", poll["inflightPipes"])
	}

	// A pipeline that is still packaging IS in-flight work.
	_, _ = store.CreatePipeline("pipeline-pack", "organization", "main", Identity{}, "queued")
	_ = store.UpdatePipeline("pipeline-pack", PipelineJob{State: PipelinePackaging})
	poll = drainGet(t, srv.URL+"/restart/poll")
	if poll["canRestart"] != false {
		t.Fatalf("a packaging pipeline must block the restart, got %v", poll)
	}
	_ = store.UpdatePipeline("pipeline-pack", PipelineJob{State: PipelineSucceeded})

	// Once the queued deploy actually starts, it is in-flight work again.
	if _, err := store.ClaimNextQueued(); err != nil {
		t.Fatalf("ClaimNextQueued: %v", err)
	}
	poll = drainGet(t, srv.URL+"/restart/poll")
	if poll["canRestart"] != false {
		t.Fatalf("a running deploy must block the restart, got %v", poll)
	}
	if n, _ := poll["inflightDeploys"].(float64); n != 1 {
		t.Fatalf("expected 1 inflight deploy, got %v", poll["inflightDeploys"])
	}

	// Deploy finished → the restart window opens again (pipeline bookkeeping is
	// the pipeline worker's job, so mark it done here too).
	_, _ = store.FinishDeploy("pipeline-3318ac5e", FinishPatch{State: StateSucceeded, Version: "cabb1e98"})
	_ = store.UpdatePipeline("pipeline-3318ac5e", PipelineJob{State: PipelineSucceeded})
	poll = drainGet(t, srv.URL+"/restart/poll")
	if poll["canRestart"] != true {
		t.Fatalf("expected canRestart=true once everything settled, got %v", poll)
	}
}
