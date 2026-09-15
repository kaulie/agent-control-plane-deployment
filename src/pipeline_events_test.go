package main

import (
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "test.sqlite"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func TestPipelineEvents(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	const id = "pipeline-evt-test"
	if _, err := s.CreatePipeline(id, "web-cursor", "main", "queued"); err != nil {
		t.Fatalf("CreatePipeline: %v", err)
	}
	if err := s.AddPipelineEvent(id, "info", "入队"); err != nil {
		t.Fatalf("AddPipelineEvent info: %v", err)
	}
	if err := s.AddPipelineEvent(id, "", "默认 info 级别"); err != nil {
		t.Fatalf("AddPipelineEvent default: %v", err)
	}
	if err := s.AddPipelineEvent(id, "success", "打包完成"); err != nil {
		t.Fatalf("AddPipelineEvent success: %v", err)
	}
	if err := s.AddPipelineEvent(id, "error", "失败原因"); err != nil {
		t.Fatalf("AddPipelineEvent error: %v", err)
	}

	events, err := s.ListPipelineEvents(id)
	if err != nil {
		t.Fatalf("ListPipelineEvents: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("want 4 events, got %d", len(events))
	}
	// ordered by insertion
	if events[0].Message != "入队" || events[0].Level != "info" {
		t.Fatalf("event0 = %+v", events[0])
	}
	if events[1].Level != "info" {
		t.Fatalf("default level should be info, got %q", events[1].Level)
	}
	if events[2].Level != "success" || events[2].Message != "打包完成" {
		t.Fatalf("event2 = %+v", events[2])
	}
	if events[3].Level != "error" {
		t.Fatalf("event3 level = %q", events[3].Level)
	}
	// monotonically increasing ids
	for i := 1; i < len(events); i++ {
		if events[i].ID <= events[i-1].ID {
			t.Fatalf("events not ordered: %d then %d", events[i-1].ID, events[i].ID)
		}
	}

	// unknown pipeline → empty list, no error
	other, err := s.ListPipelineEvents("does-not-exist")
	if err != nil {
		t.Fatalf("ListPipelineEvents missing: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("want 0 events for missing pipeline, got %d", len(other))
	}
}
