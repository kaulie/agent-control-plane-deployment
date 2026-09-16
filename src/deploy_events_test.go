package main

import (
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
	if err := s.AddDeployEvent(id, "ok", "旧版 ok 成功事件"); err != nil {
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
