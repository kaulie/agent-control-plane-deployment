package main

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/kaulie/agent-control-plane-deployment/eventlevel"
)

func TestImportLegacySQLiteOnce(t *testing.T) {
	store := newTestStore(t)
	path := filepath.Join(t.TempDir(), "deploy.sqlite")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("create sqlite: %v", err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE services (
		  service_id TEXT PRIMARY KEY, name TEXT NOT NULL, runtime_dir TEXT NOT NULL,
		  health_url TEXT NOT NULL, port INTEGER NOT NULL DEFAULT 0,
		  start_cmd TEXT NOT NULL, stop_cmd TEXT NOT NULL, restart_cmd TEXT NOT NULL,
		  watchdog_enabled INTEGER NOT NULL DEFAULT 1,
		  created_at TEXT NOT NULL, updated_at TEXT NOT NULL
		);
		CREATE TABLE deploy_events (
		  id INTEGER PRIMARY KEY,
		  request_id TEXT NOT NULL,
		  ts TEXT NOT NULL,
		  level TEXT NOT NULL,
		  message TEXT NOT NULL
		);
		CREATE TABLE meta (k TEXT PRIMARY KEY, v TEXT NOT NULL);
		INSERT INTO services (service_id, name, runtime_dir, health_url, port, start_cmd, stop_cmd, restart_cmd, created_at, updated_at)
		  VALUES ('web-cursor', 'Web', '/tmp/web-cursor', '/health', 4211, 'true', 'true', 'true', '2026-01-01T00:00:00.000Z', '2026-01-01T00:00:00.000Z');
		INSERT INTO deploy_events (id, request_id, ts, level, message)
		  VALUES (7, 'req-1', '2026-01-01T00:00:00.000Z', 'ok', 'legacy');
		INSERT INTO meta (k, v) VALUES ('supervise_v1', 'already');
	`); err != nil {
		_ = raw.Close()
		t.Fatalf("seed sqlite: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close sqlite: %v", err)
	}

	imported, err := store.ImportLegacySQLite(path)
	if err != nil || !imported {
		t.Fatalf("import = (%v, %v), want (true, nil)", imported, err)
	}
	svc, err := store.GetService("web-cursor")
	if err != nil || svc == nil || svc.Port != 4211 {
		t.Fatalf("imported service = %+v err=%v", svc, err)
	}
	events, err := store.ListDeployEvents("req-1")
	if err != nil || len(events) != 1 || events[0].ID != 7 || events[0].Level != string(eventlevel.Success) {
		t.Fatalf("imported events = %+v err=%v", events, err)
	}

	if err := store.AddDeployEvent("req-1", eventlevel.Info, "after import"); err != nil {
		t.Fatalf("AddDeployEvent: %v", err)
	}
	events, err = store.ListDeployEvents("req-1")
	if err != nil || len(events) != 2 || events[1].ID <= 7 {
		t.Fatalf("sequence after import = %+v err=%v", events, err)
	}

	again, err := store.ImportLegacySQLite(path)
	if err != nil || again {
		t.Fatalf("second import = (%v, %v), want (false, nil)", again, err)
	}
	events, _ = store.ListDeployEvents("req-1")
	if len(events) != 2 {
		t.Fatalf("second import duplicated rows: %+v", events)
	}
}

func TestImportLegacySQLiteSkipsNonEmpty(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.UpsertService(localConfig("event-center", "")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "deploy.sqlite")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE services (
		  service_id TEXT PRIMARY KEY, name TEXT NOT NULL, runtime_dir TEXT NOT NULL,
		  health_url TEXT NOT NULL, port INTEGER NOT NULL DEFAULT 0,
		  start_cmd TEXT NOT NULL, stop_cmd TEXT NOT NULL, restart_cmd TEXT NOT NULL,
		  created_at TEXT NOT NULL, updated_at TEXT NOT NULL
		);
		INSERT INTO services (service_id, name, runtime_dir, health_url, start_cmd, stop_cmd, restart_cmd, created_at, updated_at)
		  VALUES ('from-sqlite', 'From', '/tmp/from', '/health', 'true', 'true', 'true', '2026-01-01T00:00:00.000Z', '2026-01-01T00:00:00.000Z');
	`); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	_ = raw.Close()

	imported, err := store.ImportLegacySQLite(path)
	if err != nil || imported {
		t.Fatalf("import = (%v, %v), want (false, nil)", imported, err)
	}
	if svc, err := store.GetService("from-sqlite"); err != nil || svc != nil {
		t.Fatalf("non-empty db must not gain sqlite rows, svc=%v err=%v", svc, err)
	}
}

func TestImportLegacySQLiteMissingFile(t *testing.T) {
	store := newTestStore(t)
	imported, err := store.ImportLegacySQLite(filepath.Join(t.TempDir(), "missing.sqlite"))
	if err != nil || imported {
		t.Fatalf("missing file = (%v, %v), want (false, nil)", imported, err)
	}
}
