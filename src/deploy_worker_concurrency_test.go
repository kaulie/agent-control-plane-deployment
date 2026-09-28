package main

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newDeployWorkerTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(filepath.Join(t.TempDir(), "deploys.sqlite"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func mustService(t *testing.T, store *Store, id, runtimeDir string) {
	t.Helper()
	if _, err := store.UpsertService(ServiceContract{
		ServiceID: id, Name: id, RuntimeDir: runtimeDir,
		HealthURL: "http://127.0.0.1:1/health",
		StartCmd:  "true", StopCmd: "true", RestartCmd: "true",
	}); err != nil {
		t.Fatalf("UpsertService(%s): %v", id, err)
	}
}

func mustQueuedDeploy(t *testing.T, store *Store, requestID, serviceID string) {
	t.Helper()
	if _, err := store.CreateDeploy(requestID, serviceID, "deployment-"+requestID,
		Identity{}, "queued"); err != nil {
		t.Fatalf("CreateDeploy(%s): %v", requestID, err)
	}
}

// gatedRunner records which deploys were started and blocks each of them until
// its gate is closed — so a test can assert what happens *while* a deploy is
// still running.
type gatedRunner struct {
	mu      sync.Mutex
	started []string
	gates   map[string]chan struct{}
}

func newGatedRunner(requestIDs ...string) *gatedRunner {
	gates := make(map[string]chan struct{}, len(requestIDs))
	for _, id := range requestIDs {
		gates[id] = make(chan struct{})
	}
	return &gatedRunner{gates: gates}
}

func (g *gatedRunner) run(requestID string) {
	g.mu.Lock()
	g.started = append(g.started, requestID)
	g.mu.Unlock()
	<-g.gates[requestID]
}

func (g *gatedRunner) release(requestID string) {
	close(g.gates[requestID])
}

func (g *gatedRunner) startedCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.started)
}

func waitForStarted(t *testing.T, g *gatedRunner, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if g.startedCount() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("only %d deploy(s) started, want %d (deploys of different services must not queue behind each other)",
		g.startedCount(), want)
}

func deployState(t *testing.T, store *Store, requestID string) DeployState {
	t.Helper()
	job, err := store.GetDeploy(requestID)
	if err != nil || job == nil {
		t.Fatalf("GetDeploy(%s): job=%v err=%v", requestID, job, err)
	}
	return job.State
}

func waitForDeployUnitReleased(t *testing.T, worker *DeployWorker, unit string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		worker.mu.Lock()
		_, busy := worker.active[unit]
		worker.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("runtime %s still marked busy after its deploy finished", unit)
}

// A deploy waiting in the peer's graceful restart window (up to 10 minutes)
// used to block every other service, because the worker executed exactly one
// deploy at a time: the second service's deploy stayed `queued` while its
// pipeline showed `deploying` the whole time. Deploys of different services are
// independent, so the second one must start immediately.
func TestDeployWorkerRunsUnrelatedServicesConcurrently(t *testing.T) {
	store := newDeployWorkerTestStore(t)
	mustService(t, store, "autonomy", "/tmp/rt-autonomy")
	mustService(t, store, "agent-control-plane", "/tmp/rt-agent-control-plane")
	mustQueuedDeploy(t, store, "pipeline-blocked", "agent-control-plane")
	mustQueuedDeploy(t, store, "pipeline-behind", "autonomy")

	worker := NewDeployWorker(store, Config{}, nil, &GracefulDrain{}, nil, nil)
	runner := newGatedRunner("pipeline-blocked", "pipeline-behind")
	worker.run = runner.run

	worker.tick()
	// "pipeline-blocked" sits in its graceful window; "pipeline-behind" must
	// have been claimed and started anyway.
	waitForStarted(t, runner, 2)
	if got := deployState(t, store, "pipeline-behind"); got != StateRunning {
		t.Fatalf("pipeline-behind state = %s, want running", got)
	}

	runner.release("pipeline-blocked")
	runner.release("pipeline-behind")
	worker.deployWg.Wait()
}

// Two contracts can point at the same runtime dir (the seeded `web-cursor` and
// the registry `agent-control-plane` both deploy ~/runtime/web-cursor). Those
// must never rsync/restart one runtime at the same time: the older queued
// deploy runs first and the next one waits for it.
func TestDeployWorkerSerializesSameRuntimeDir(t *testing.T) {
	store := newDeployWorkerTestStore(t)
	shared := "/tmp/rt-web-cursor"
	mustService(t, store, "web-cursor", shared)
	mustService(t, store, "agent-control-plane", shared)
	mustQueuedDeploy(t, store, "pipeline-first", "web-cursor")
	time.Sleep(5 * time.Millisecond) // keep requested_at strictly ordered
	mustQueuedDeploy(t, store, "pipeline-second", "agent-control-plane")

	worker := NewDeployWorker(store, Config{}, nil, &GracefulDrain{}, nil, nil)
	runner := newGatedRunner("pipeline-first", "pipeline-second")
	worker.run = runner.run

	worker.tick()
	waitForStarted(t, runner, 1)
	if got := deployState(t, store, "pipeline-first"); got != StateRunning {
		t.Fatalf("pipeline-first state = %s, want running", got)
	}
	if got := deployState(t, store, "pipeline-second"); got != StateQueued {
		t.Fatalf("pipeline-second state = %s, want queued while the same runtime is busy", got)
	}

	runner.release("pipeline-first")
	waitForDeployUnitReleased(t, worker, shared)

	worker.tick()
	waitForStarted(t, runner, 2)
	if got := deployState(t, store, "pipeline-second"); got != StateRunning {
		t.Fatalf("pipeline-second state = %s, want running after the first finished", got)
	}
	runner.release("pipeline-second")
	worker.deployWg.Wait()
}

// While the control plane restarts itself (drain) the worker must not claim
// anything new — a queued deploy would otherwise start right before the process
// is killed.
func TestDeployWorkerDoesNotClaimWhileDraining(t *testing.T) {
	store := newDeployWorkerTestStore(t)
	mustService(t, store, "autonomy", "/tmp/rt-autonomy")
	mustQueuedDeploy(t, store, "pipeline-later", "autonomy")

	drain := &GracefulDrain{}
	drain.Notify("pipeline-self")
	worker := NewDeployWorker(store, Config{}, nil, drain, nil, nil)
	runner := newGatedRunner("pipeline-later")
	worker.run = runner.run

	worker.tick()
	if got := deployState(t, store, "pipeline-later"); got != StateQueued {
		t.Fatalf("pipeline-later state = %s, want queued while draining", got)
	}
	if n := runner.startedCount(); n != 0 {
		t.Fatalf("%d deploy(s) started while draining, want 0", n)
	}

	drain.Clear()
	worker.tick()
	waitForStarted(t, runner, 1)
	runner.release("pipeline-later")
	worker.deployWg.Wait()
}
