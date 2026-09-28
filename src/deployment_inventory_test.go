package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func fakeRegistrySnapshot(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/services":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"services":[{"namespace":"default","name":"web-cursor"}],"total":1}`))
		case "/v1/snapshot":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDeploymentInventoryMergesRegistryAndDeploys(t *testing.T) {
	srv := fakeRegistrySnapshot(t, `{"services":[{"namespace":"default","name":"web-cursor"}],
		"instances":[
		  {"namespace":"default","service":"web-cursor","host":"127.0.0.1","port":4211,"metadata":{"machine":"local"}},
		  {"namespace":"default","service":"web-cursor","host":"10.0.0.8","port":4211,"metadata":{"machine":"gpu-2"}}
		]}`)
	reg := registryClientFor(srv.URL)
	store, err := NewStore(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.UpsertService(ServiceContract{ServiceID: "web-cursor", Name: "Web", RuntimeDir: "/tmp/w"}); err != nil {
		t.Fatalf("UpsertService: %v", err)
	}
	by := Identity{Role: "user", ID: "u1"}
	if _, err := store.CreateDeploy("d-local", "web-cursor", "dep-a", by, "ok", "local"); err != nil {
		t.Fatalf("CreateDeploy local: %v", err)
	}
	if _, err := store.FinishDeploy("d-local", FinishPatch{State: StateSucceeded, Version: "v100", Message: "done"}); err != nil {
		t.Fatalf("FinishDeploy local: %v", err)
	}
	if _, err := store.CreateDeploy("d-gpu", "web-cursor", "dep-b", by, "ok", "gpu-2"); err != nil {
		t.Fatalf("CreateDeploy gpu: %v", err)
	}
	if _, err := store.FinishDeploy("d-gpu", FinishPatch{State: StateSucceeded, Version: "v99", Message: "done"}); err != nil {
		t.Fatalf("FinishDeploy gpu: %v", err)
	}

	inv, err := buildDeploymentInventory(context.Background(), store, reg, defaultDeployMachine)
	if err != nil {
		t.Fatalf("buildDeploymentInventory: %v", err)
	}
	if len(inv.Services) != 1 {
		t.Fatalf("services = %d, want 1", len(inv.Services))
	}
	svc := inv.Services[0]
	if len(svc.Machines) != 2 {
		t.Fatalf("machines = %d, want 2", len(svc.Machines))
	}
	drift := 0
	for _, m := range svc.Machines {
		if m.VersionDrift {
			drift++
		}
	}
	if drift != 1 {
		t.Fatalf("expected exactly one drift row, got %d (%+v)", drift, svc.Machines)
	}
}

func TestDeploymentInventoryHTTP(t *testing.T) {
	api, store := newTestIdentityServer(t, false)
	srv := httptest.NewServer(api.routes())
	t.Cleanup(srv.Close)
	t.Cleanup(func() { _ = store.Close() })

	resp, err := http.Get(srv.URL + "/api/deployment-inventory")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var inv DeploymentInventory
	if err := json.NewDecoder(resp.Body).Decode(&inv); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if inv.DefaultDeployMachine == "" {
		t.Fatal("defaultDeployMachine must be set")
	}
}
