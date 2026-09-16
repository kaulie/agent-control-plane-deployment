package main

import (
	"net/http/httptest"
	"testing"
)

func TestParseListFilter(t *testing.T) {
	r := httptest.NewRequest("GET",
		"/api/deploys?serviceId=web&state=failed&triggeredByRole=agent&triggeredById=a1"+
			"&ref=main&deployment=aaa&version=v1&q=abc&from=2026-01-01&to=2026-02-01&page=3&pageSize=50", nil)
	f := parseListFilter(r)
	if f.ServiceID != "web" || f.State != "failed" || f.TriggeredByRole != "agent" ||
		f.TriggeredByID != "a1" || f.Ref != "main" || f.Deployment != "aaa" || f.Version != "v1" ||
		f.Keyword != "abc" || f.From != "2026-01-01" || f.To != "2026-02-01" ||
		f.Page != 3 || f.PageSize != 50 {
		t.Fatalf("parsed = %+v", f)
	}

	// defaults: page 1, default page size
	def := parseListFilter(httptest.NewRequest("GET", "/api/deploys", nil))
	if def.Page != 1 || def.PageSize != defaultPageSize {
		t.Fatalf("defaults = %+v", def)
	}

	// "limit" stays a pageSize alias, and pageSize is clamped
	lim := parseListFilter(httptest.NewRequest("GET", "/api/deploys?limit=9999", nil))
	if lim.PageSize != maxPageSize {
		t.Fatalf("limit alias/clamp = %d, want %d", lim.PageSize, maxPageSize)
	}
}

func TestListDeploysFiltered(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	seed := []struct{ id, svc, dep, state, role, uid, version string }{
		{"d1", "web-cursor", "deployment-aaa", "succeeded", "user", "user_001", "v1"},
		{"d2", "web-cursor", "deployment-bbb", "failed", "agent", "agent_002", "v1"},
		{"d3", "acp", "deployment-ccc", "succeeded", "user", "user_002", "v2"},
		{"d4", "web-cursor", "deployment-aaa", "queued", "agent", "agent_002", ""},
	}
	for _, x := range seed {
		if _, err := s.CreateDeploy(x.id, x.svc, x.dep, Identity{Role: x.role, ID: x.uid}, "m "+x.id); err != nil {
			t.Fatalf("CreateDeploy %s: %v", x.id, err)
		}
		if _, err := s.FinishDeploy(x.id, FinishPatch{State: DeployState(x.state), Version: x.version}); err != nil {
			t.Fatalf("FinishDeploy %s: %v", x.id, err)
		}
	}
	// Give each row a distinct requested_at so time-range filters are meaningful.
	stamps := map[string]string{
		"d1": "2026-01-01T10:00:00.000Z",
		"d2": "2026-02-01T10:00:00.000Z",
		"d3": "2026-03-01T10:00:00.000Z",
		"d4": "2026-04-01T10:00:00.000Z",
	}
	for id, ts := range stamps {
		if _, err := s.db.Exec(`UPDATE deploys SET requested_at = ? WHERE request_id = ?`, ts, id); err != nil {
			t.Fatalf("stamp requested_at: %v", err)
		}
	}

	check := func(name string, f ListFilter, wantTotal int, wantIDs ...string) {
		t.Helper()
		jobs, total, err := s.ListDeploysFiltered(f)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if total != wantTotal {
			t.Fatalf("%s: total = %d, want %d", name, total, wantTotal)
		}
		if len(wantIDs) > 0 && len(jobs) != len(wantIDs) {
			t.Fatalf("%s: got %d rows, want %d", name, len(jobs), len(wantIDs))
		}
		for i, id := range wantIDs {
			if jobs[i].RequestID != id {
				t.Fatalf("%s: row %d = %s, want %s", name, i, jobs[i].RequestID, id)
			}
		}
	}

	// no filters: newest first
	check("all", ListFilter{}, 4, "d4", "d3", "d2", "d1")
	check("serviceId", ListFilter{ServiceID: "web-cursor"}, 3)
	check("state", ListFilter{State: "succeeded"}, 2)
	check("identity", ListFilter{TriggeredByRole: "agent", TriggeredByID: "agent_002"}, 2)
	check("deployment contains", ListFilter{Deployment: "aaa"}, 2)
	check("keyword", ListFilter{Keyword: "d2"}, 1, "d2")
	check("time range Feb..Mar", ListFilter{
		From: "2026-02-01T00:00:00.000Z",
		To:   "2026-03-31T23:59:59.999Z",
	}, 2, "d3", "d2")
	// combined filters
	check("failed+web", ListFilter{ServiceID: "web-cursor", State: "failed"}, 1, "d2")

	// pagination: pageSize 2
	check("page1", ListFilter{Page: 1, PageSize: 2}, 4, "d4", "d3")
	check("page2", ListFilter{Page: 2, PageSize: 2}, 4, "d2", "d1")
	check("page3 beyond end", ListFilter{Page: 3, PageSize: 2}, 4)
}

func TestListPipelinesFiltered(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	seed := []struct{ id, svc, ref, state, role, uid string }{
		{"p1", "web-cursor", "main", "succeeded", "user", "user_001"},
		{"p2", "web-cursor", "feature/x", "failed", "agent", "agent_002"},
		{"p3", "acp", "main", "deploying", "agent", "agent_002"},
	}
	for _, x := range seed {
		if _, err := s.CreatePipeline(x.id, x.svc, x.ref, Identity{Role: x.role, ID: x.uid}, "m "+x.id); err != nil {
			t.Fatalf("CreatePipeline %s: %v", x.id, err)
		}
		if err := s.UpdatePipeline(x.id, PipelineJob{State: PipelineState(x.state)}); err != nil {
			t.Fatalf("UpdatePipeline %s: %v", x.id, err)
		}
	}

	if _, total, err := s.ListPipelinesFiltered(ListFilter{}); err != nil || total != 3 {
		t.Fatalf("all: total=%d err=%v", total, err)
	}
	// ref is pipeline-only and uses a contains match
	jobs, total, err := s.ListPipelinesFiltered(ListFilter{Ref: "feature"})
	if err != nil || total != 1 || len(jobs) != 1 || jobs[0].RequestID != "p2" {
		t.Fatalf("ref filter: total=%d jobs=%+v err=%v", total, jobs, err)
	}
	if _, total, _ := s.ListPipelinesFiltered(ListFilter{State: "deploying"}); total != 1 {
		t.Fatalf("state filter total=%d, want 1", total)
	}
	if _, total, _ := s.ListPipelinesFiltered(ListFilter{ServiceID: "web-cursor"}); total != 2 {
		t.Fatalf("serviceId filter total=%d, want 2", total)
	}
	if p1, total, _ := s.ListPipelinesFiltered(ListFilter{Page: 1, PageSize: 2}); total != 3 || len(p1) != 2 {
		t.Fatalf("paging: total=%d rows=%d", total, len(p1))
	}
}
