package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
)

func TestIntervalSecDefaultsAndPersists(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "interval.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	svc := localConfig("event-center", "")
	saved, err := store.UpsertService(svc)
	if err != nil {
		t.Fatal(err)
	}
	if saved.IntervalSec != 30 {
		t.Fatalf("new contract default intervalSec=%d, want 30", saved.IntervalSec)
	}
	got, err := store.GetService("event-center")
	if err != nil || got == nil {
		t.Fatalf("GetService: %v %#v", err, got)
	}
	if got.IntervalSec != 30 {
		t.Fatalf("stored intervalSec=%d, want 30", got.IntervalSec)
	}

	api := &apiServer{store: store}
	rec := putServiceJSON(t, api, "event-center", `{
		"runtimeDir":"/tmp/event-center","healthUrl":"/health","port":4438,
		"startCmd":"true","stopCmd":"true","restartCmd":"true","intervalSec":45
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT intervalSec=45 = %d %s", rec.Code, rec.Body.String())
	}
	var body ServiceContract
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.IntervalSec != 45 {
		t.Fatalf("PUT response intervalSec=%d", body.IntervalSec)
	}
	again, _ := store.GetService("event-center")
	if again == nil || again.IntervalSec != 45 {
		t.Fatal("intervalSec did not persist")
	}
}

func TestPutServiceKeepsIntervalWhenOmitted(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "keep-interval.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := localConfig("event-center", "")
	svc.IntervalSec = 45
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
	if saved == nil || saved.IntervalSec != 45 {
		t.Fatal("omitting intervalSec must keep the existing value")
	}
}

func TestPutServiceRejectsInvalidInterval(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "bad-interval.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.UpsertService(localConfig("event-center", "")); err != nil {
		t.Fatal(err)
	}
	api := &apiServer{store: store}
	for _, raw := range []string{"1", "0", "86401"} {
		rec := putServiceJSON(t, api, "event-center", `{
			"runtimeDir":"/tmp/event-center","healthUrl":"/health","port":4438,
			"startCmd":"true","stopCmd":"true","restartCmd":"true","intervalSec":`+raw+`
		}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("intervalSec=%s → %d, want 400", raw, rec.Code)
		}
	}
}
