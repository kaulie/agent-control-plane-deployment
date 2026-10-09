package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
)

func TestSupervisePersistsAndSeedsFirstWave(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "deploy.sqlite"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	if _, err := store.UpsertService(localConfig("home-agent-brain", "")); err != nil {
		t.Fatalf("UpsertService: %v", err)
	}
	if _, err := store.UpsertService(localConfig("event-center", "")); err != nil {
		t.Fatalf("UpsertService: %v", err)
	}
	if _, err := store.db.Exec(`DELETE FROM meta WHERE k = 'supervise_v1'`); err != nil {
		t.Fatal(err)
	}
	if err := store.migrateSuperviseDefaultsOnce(); err != nil {
		t.Fatal(err)
	}

	brain, err := store.GetService("home-agent-brain")
	if err != nil || brain == nil {
		t.Fatalf("brain: %v %#v", err, brain)
	}
	if !brain.Supervise {
		t.Fatal("first-wave home-agent-brain should be supervised after migrate")
	}
	ec, err := store.GetService("event-center")
	if err != nil || ec == nil {
		t.Fatalf("event-center: %v", err)
	}
	if ec.Supervise {
		t.Fatal("event-center must stay off until an operator checks the box")
	}

	api := &apiServer{store: store}
	rec := putServiceJSON(t, api, "event-center", `{
		"runtimeDir":"/tmp/event-center","healthUrl":"/health","port":4438,
		"startCmd":"true","stopCmd":"true","restartCmd":"true","supervise":true
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT supervise=true = %d %s", rec.Code, rec.Body.String())
	}
	var got ServiceContract
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Supervise {
		t.Fatalf("PUT response supervise=%v", got.Supervise)
	}
	saved, _ := store.GetService("event-center")
	if saved == nil || !saved.Supervise {
		t.Fatal("supervise did not persist")
	}
}

func TestPutServiceKeepsSuperviseWhenOmitted(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "keep.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := localConfig("event-center", "")
	svc.Supervise = true
	if _, err := store.UpsertService(svc); err != nil {
		t.Fatal(err)
	}
	api := &apiServer{store: store}
	rec := putServiceJSON(t, api, "event-center", `{
		"runtimeDir":"/tmp/event-center","healthUrl":"/health","port":4438,
		"startCmd":"true","stopCmd":"true","restartCmd":"true"
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body.String())
	}
	saved, _ := store.GetService("event-center")
	if saved == nil || !saved.Supervise {
		t.Fatal("omitting supervise must keep the existing value")
	}
}
