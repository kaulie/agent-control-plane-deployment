package main

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/kaulie/agent-control-plane-deployment/eventlevel"
)

func TestDeployEvents(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "test.sqlite"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer s.Close()

	const id = "deploy-evt-test"
	if _, err := s.CreateDeploy(id, "web-cursor", "deployment-abc12345", Identity{},
		"queued"); err != nil {
		t.Fatalf("CreateDeploy: %v", err)
	}
	if err := s.AddDeployEvent(id, eventlevel.Info, "开始部署"); err != nil {
		t.Fatalf("AddDeployEvent info: %v", err)
	}
	if err := s.AddDeployEvent(id, "", "默认 info"); err != nil {
		t.Fatalf("AddDeployEvent default: %v", err)
	}
	if err := s.AddDeployEvent(id, eventlevel.Success, "部署成功"); err != nil {
		t.Fatalf("AddDeployEvent success: %v", err)
	}

	events, err := s.ListDeployEvents(id)
	if err != nil {
		t.Fatalf("ListDeployEvents: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("want 3 events, got %d", len(events))
	}
	if events[0].Message != "开始部署" || events[0].Level != string(eventlevel.Info) {
		t.Fatalf("event0 = %+v", events[0])
	}
	if events[1].Level != string(eventlevel.Info) {
		t.Fatalf("default level should be info, got %q", events[1].Level)
	}
	if events[2].Level != string(eventlevel.Success) || events[2].Message != "部署成功" {
		t.Fatalf("event2 = %+v", events[2])
	}
	for i := 1; i < len(events); i++ {
		if events[i].ID <= events[i-1].ID {
			t.Fatalf("events not ordered: %d then %d", events[i-1].ID, events[i].ID)
		}
	}

	other, err := s.ListDeployEvents("does-not-exist")
	if err != nil {
		t.Fatalf("ListDeployEvents missing: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("want 0 events for missing deploy, got %d", len(other))
	}
}

func TestDeployEventLevelNormalization(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "test.sqlite"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer s.Close()

	const id = "deploy-evt-level-normalization"
	if _, err := s.CreateDeploy(id, "web-cursor", "deployment-abc12345", Identity{},
		"queued"); err != nil {
		t.Fatalf("CreateDeploy: %v", err)
	}
	if err := s.AddDeployEvent(id, eventlevel.LegacySuccessAlias, "旧版 ok 成功事件"); err != nil {
		t.Fatalf("AddDeployEvent legacy ok: %v", err)
	}

	events, err := s.ListDeployEvents(id)
	if err != nil {
		t.Fatalf("ListDeployEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	if events[0].Level != string(eventlevel.Success) {
		t.Fatalf("legacy ok should normalize to success, got %q", events[0].Level)
	}
}

func TestEventLevelMigrationNormalizesLegacyAndUnknownLevels(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.sqlite")

	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	for _, stmt := range []string{
		`CREATE TABLE deploy_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			request_id TEXT NOT NULL,
			ts TEXT NOT NULL,
			level TEXT NOT NULL DEFAULT 'info',
			message TEXT NOT NULL
		)`,
		`CREATE TABLE pipeline_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			request_id TEXT NOT NULL,
			ts TEXT NOT NULL,
			level TEXT NOT NULL DEFAULT 'info',
			message TEXT NOT NULL
		)`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			_ = raw.Close()
			t.Fatalf("create raw event table: %v", err)
		}
	}

	insert := func(table, requestID string, levels []string) {
		t.Helper()
		for _, level := range levels {
			if _, err := raw.Exec(
				`INSERT INTO `+table+` (request_id, ts, level, message) VALUES (?, ?, ?, ?)`,
				requestID, nowISO(), level, "legacy "+level,
			); err != nil {
				_ = raw.Close()
				t.Fatalf("insert %s level %q: %v", table, level, err)
			}
		}
	}
	insert("deploy_events", "legacy-deploy", []string{"ok", "custom", "", "info", "warn", "error"})
	insert("pipeline_events", "legacy-pipeline", []string{"ok", "custom", "success", "error"})
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer s.Close()

	assertLevels := func(table, requestID string, want []string) {
		t.Helper()
		rows, err := s.db.Query(
			`SELECT level FROM `+table+` WHERE request_id = ? ORDER BY id ASC`,
			requestID,
		)
		if err != nil {
			t.Fatalf("query %s levels: %v", table, err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var level string
			if err := rows.Scan(&level); err != nil {
				t.Fatalf("scan %s level: %v", table, err)
			}
			got = append(got, level)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate %s levels: %v", table, err)
		}
		if len(got) != len(want) {
			t.Fatalf("%s levels = %v, want %v", table, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s levels = %v, want %v", table, got, want)
			}
		}
	}
	assertLevels("deploy_events", "legacy-deploy",
		[]string{string(eventlevel.Success), string(eventlevel.Info), string(eventlevel.Info),
			string(eventlevel.Info), string(eventlevel.Warn), string(eventlevel.Error)})
	assertLevels("pipeline_events", "legacy-pipeline",
		[]string{string(eventlevel.Success), string(eventlevel.Info),
			string(eventlevel.Success), string(eventlevel.Error)})
}
