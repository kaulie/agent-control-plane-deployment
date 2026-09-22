package main

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// 老库（pipelines 表还没有 use_proxy 列）打开时必须自动补列，并且和
// pipelineColumns 的 SELECT 列表对得上 —— 否则升级后所有流水线读取都会失败。
func TestPipelineMigrationAddsUseProxyColumn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "old.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// 升级前的表结构（没有 use_proxy）+ 一条老记录。
	if _, err := db.Exec(`
      CREATE TABLE pipelines (
        request_id TEXT PRIMARY KEY,
        service_id TEXT NOT NULL,
        ref TEXT NOT NULL,
        state TEXT NOT NULL,
        deployment TEXT,
        deploy_request_id TEXT,
        version TEXT,
        error TEXT,
        message TEXT,
        requested_at TEXT NOT NULL,
        started_at TEXT,
        finished_at TEXT,
        triggered_by_role TEXT NOT NULL DEFAULT '',
        triggered_by_id TEXT NOT NULL DEFAULT ''
      );
      INSERT INTO pipelines (request_id, service_id, ref, state, requested_at)
        VALUES ('pipeline-old', 'web-cursor', 'main', 'succeeded', '2026-09-20T00:00:00.000Z');
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

	old, err := store.GetPipeline("pipeline-old")
	if err != nil || old == nil {
		t.Fatalf("GetPipeline(old row): job=%v err=%v", old, err)
	}
	if old.UseProxy {
		t.Fatalf("old rows default to 直连, got %+v", old)
	}
	if old.State != PipelineSucceeded || old.ServiceID != "web-cursor" {
		t.Fatalf("old row read back wrong: %+v", old)
	}

	// 新记录带上选项，和迁移过来的老行共存。
	if _, err := store.CreatePipeline("pipeline-new", "web-cursor", "main", true, Identity{}, "queued"); err != nil {
		t.Fatalf("CreatePipeline: %v", err)
	}
	fresh, err := store.GetPipeline("pipeline-new")
	if err != nil || fresh == nil {
		t.Fatalf("GetPipeline(new row): job=%v err=%v", fresh, err)
	}
	if !fresh.UseProxy {
		t.Fatalf("useProxy must round-trip, got %+v", fresh)
	}
}
