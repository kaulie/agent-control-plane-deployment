package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseIdentity(t *testing.T) {
	cases := []struct {
		name    string
		role    string
		id      string
		want    Identity
		wantErr string
	}{
		{name: "user", role: "user", id: "user_001", want: Identity{Role: "user", ID: "user_001"}},
		{name: "agent", role: "agent", id: "agent_002", want: Identity{Role: "agent", ID: "agent_002"}},
		{name: "trims and lowercases role", role: " Agent ", id: " agent_002 ",
			want: Identity{Role: "agent", ID: "agent_002"}},
		{name: "missing both", wantErr: "missing identity"},
		{name: "missing id", role: "user", wantErr: "missing identity"},
		{name: "missing role", id: "user_001", wantErr: "missing identity"},
		{name: "unknown role", role: "robot", id: "r1", wantErr: "invalid identity_role"},
		{name: "id too long", role: "user", id: strings.Repeat("a", maxIdentityIDLen+1), wantErr: "too long"},
		{name: "id with whitespace", role: "user", id: "user 001", wantErr: "whitespace"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseIdentity(tc.role, tc.id)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestIdentityStringAndKnown(t *testing.T) {
	id := Identity{Role: "user", ID: "user_001"}
	if got := id.String(); got != "user:user_001" {
		t.Fatalf("String() = %q", got)
	}
	if !id.Known() {
		t.Fatal("Known() should be true for a filled identity")
	}
	if (Identity{}).Known() {
		t.Fatal("Known() should be false for the zero identity")
	}
	if got := (Identity{}).String(); got != "" {
		t.Fatalf("zero identity String() = %q, want empty", got)
	}
}

// newTestIdentityServer returns a server backed by a temp store + temp local
// artifact storage holding one existing artifact (so /api/deploys passes the
// artifact check). worker/pipeline stay nil: the handlers only record the job
// (both have nil-guards), so no deploy/packaging is executed in tests.
func newTestIdentityServer(t *testing.T, enforce bool) (*apiServer, *Store) {
	t.Helper()
	dir := t.TempDir()
	store, err := NewStore(filepath.Join(dir, "test.sqlite"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	pkgs := filepath.Join(dir, "packages")
	pkgDir := filepath.Join(pkgs, "web-cursor", "deployment-abc12345")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatalf("mkdir package: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "VERSION"), []byte("abc12345\n"), 0o644); err != nil {
		t.Fatalf("write VERSION: %v", err)
	}
	cfg := Config{
		Home:                dir,
		PackagesDir:         pkgs,
		ArtifactStorageType: "local",
		IdentityEnforce:     enforce,
	}
	storage, err := NewArtifactStorage(cfg)
	if err != nil {
		t.Fatalf("NewArtifactStorage: %v", err)
	}
	svc := ServiceContract{
		ServiceID:  "web-cursor",
		Name:       "web-cursor",
		RuntimeDir: filepath.Join(dir, "runtime-web-cursor"),
		HealthURL:  "http://127.0.0.1:1/health",
		StartCmd:   "true",
		StopCmd:    "true",
		RestartCmd: "true",
		GitRepoURL: "https://github.com/kaulie/web-cursor",
	}
	if _, err := store.UpsertService(svc); err != nil {
		t.Fatalf("UpsertService: %v", err)
	}
	return &apiServer{store: store, cfg: cfg, storage: storage}, store
}

func postJSON(t *testing.T, srv *apiServer, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

func TestDeployAPIsRequireIdentity(t *testing.T) {
	srv, _ := newTestIdentityServer(t, true)
	cases := []struct {
		path string
		body string
	}{
		{"/api/deploys", `{"serviceId":"web-cursor","deployment":"deployment-abc12345"}`},
		{"/api/deploy-notify", `{"serviceId":"web-cursor","ref":"main"}`},
	}
	badHeaders := []map[string]string{
		{},                             // no identity at all
		{identityRoleHeader: "user"},   // id missing
		{identityIDHeader: "user_001"}, // role missing
		{identityRoleHeader: "robot", identityIDHeader: "r1"},                                   // unknown role
		{identityRoleHeader: "user", identityIDHeader: strings.Repeat("a", maxIdentityIDLen+1)}, // too long
	}
	for _, tc := range cases {
		for i, h := range badHeaders {
			rec := postJSON(t, srv, tc.path, tc.body, h)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s headers#%d: status = %d, want 401 (body=%s)",
					tc.path, i, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "identity") {
				t.Fatalf("%s headers#%d: error should mention identity, got %s",
					tc.path, i, rec.Body.String())
			}
		}
	}
}

func TestDeployAPIsAllowMissingIdentityWhenEnforcementOff(t *testing.T) {
	srv, _ := newTestIdentityServer(t, false)
	// No identity headers: the gate lets the request through, so the handlers
	// fail on ordinary validation (empty body → missing serviceId), not 401.
	for _, path := range []string{"/api/deploys", "/api/deploy-notify"} {
		rec := postJSON(t, srv, path, `{}`, nil)
		if rec.Code == http.StatusUnauthorized {
			t.Fatalf("%s: still 401 although enforcement is off (body=%s)", path, rec.Body.String())
		}
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400 (body=%s)", path, rec.Code, rec.Body.String())
		}
	}
}

func TestCreateDeployRecordsIdentity(t *testing.T) {
	srv, store := newTestIdentityServer(t, true)
	rec := postJSON(t, srv, "/api/deploys",
		`{"serviceId":"web-cursor","deployment":"deployment-abc12345","requestId":"deploy-req-by"}`,
		map[string]string{identityRoleHeader: "user", identityIDHeader: "user_001"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body=%s)", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["triggeredBy"] != "user:user_001" || resp["triggeredByRole"] != "user" || resp["triggeredById"] != "user_001" {
		t.Fatalf("response identity fields = %v", resp)
	}
	job, err := store.GetDeploy("deploy-req-by")
	if err != nil || job == nil {
		t.Fatalf("GetDeploy: job=%v err=%v", job, err)
	}
	if job.Identity() != (Identity{Role: "user", ID: "user_001"}) {
		t.Fatalf("stored identity = %+v", job)
	}
	listed, err := store.ListDeploys(10)
	if err != nil || len(listed) != 1 {
		t.Fatalf("ListDeploys: %v err=%v", listed, err)
	}
	if listed[0].TriggeredBy != "user:user_001" {
		t.Fatalf("listed triggeredBy = %q", listed[0].TriggeredBy)
	}
}

func TestDeployNotifyRecordsIdentity(t *testing.T) {
	srv, store := newTestIdentityServer(t, true)
	rec := postJSON(t, srv, "/api/deploy-notify",
		`{"serviceId":"web-cursor","ref":"main","requestId":"pipeline-by"}`,
		map[string]string{identityRoleHeader: "agent", identityIDHeader: "agent_002"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body=%s)", rec.Code, rec.Body.String())
	}
	job, err := store.GetPipeline("pipeline-by")
	if err != nil || job == nil {
		t.Fatalf("GetPipeline: job=%v err=%v", job, err)
	}
	if job.Identity() != (Identity{Role: "agent", ID: "agent_002"}) || job.TriggeredBy != "agent:agent_002" {
		t.Fatalf("stored pipeline identity = %+v", job)
	}
	// The enqueue event carries the triggerer too.
	events, err := store.ListPipelineEvents("pipeline-by")
	if err != nil || len(events) == 0 {
		t.Fatalf("ListPipelineEvents: %v err=%v", events, err)
	}
	if !strings.Contains(events[0].Message, "触发者=agent:agent_002") {
		t.Fatalf("enqueue event = %q", events[0].Message)
	}
}
