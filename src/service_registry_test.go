package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeServiceRegistry serves GET /v1/services (the same shape as
// service-registry's catalog endpoint).
func fakeServiceRegistry(t *testing.T, services []RegistryService) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/services" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"services": services, "total": len(services)})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func registryClientFor(url string) *ServiceRegistry {
	return NewServiceRegistry(Config{ServiceRegistryURL: url, ServiceRegistryTimeout: 3 * time.Second})
}

func TestServiceRegistryDisabledIsNilSafe(t *testing.T) {
	reg := NewServiceRegistry(Config{})
	if reg.Enabled() {
		t.Fatal("empty SERVICE_REGISTRY_URL must yield a disabled client")
	}
	if reg.BaseURL() != "" {
		t.Fatalf("BaseURL() = %q, want empty", reg.BaseURL())
	}
	// nil receiver must not panic (it is "cannot verify", not "no services").
	var nilReg *ServiceRegistry
	if nilReg.Enabled() || nilReg.BaseURL() != "" {
		t.Fatal("nil registry should report disabled")
	}
	if got := resolveServiceGitRepo(context.Background(), nilReg, &ServiceContract{ServiceID: "x"}); got != "" {
		t.Fatalf("resolveServiceGitRepo with nil registry = %q, want empty", got)
	}
}

func TestServiceRegistryList(t *testing.T) {
	srv := fakeServiceRegistry(t, []RegistryService{
		{Namespace: "default", Name: "service_registry", Version: "0.0.1", Owner: "kaulie",
			GitRepoURL: "https://github.com/kaulie/service-registry"},
		{Namespace: "team-a", Name: "event-center", Version: "1.4.2"},
	})
	reg := registryClientFor(srv.URL)
	svcs, err := reg.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(svcs) != 2 {
		t.Fatalf("want 2 services, got %d", len(svcs))
	}
	if svcs[0].Name != "service_registry" || svcs[0].GitRepoURL != "https://github.com/kaulie/service-registry" {
		t.Fatalf("unexpected first service: %+v", svcs[0])
	}
	svc, found, err := reg.Lookup(context.Background(), "event-center")
	if err != nil || !found {
		t.Fatalf("Lookup(event-center) = %v, %v", found, err)
	}
	if svc.Version != "1.4.2" {
		t.Fatalf("version = %q", svc.Version)
	}
	if _, found, _ := reg.Lookup(context.Background(), "nope"); found {
		t.Fatal("Lookup(nope) should report not found")
	}
}

func TestServiceRegistryErrors(t *testing.T) {
	// Unreachable registry → error (never "no services").
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	reg := registryClientFor(deadURL)
	if _, err := reg.List(context.Background()); err == nil {
		t.Fatal("expected error for unreachable registry")
	}

	// Non-200 with the registry's error envelope → message surfaces.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"需要令牌"}}`))
	}))
	defer bad.Close()
	_, err := registryClientFor(bad.URL).List(context.Background())
	if err == nil || !strings.Contains(err.Error(), "forbidden") || !strings.Contains(err.Error(), "需要令牌") {
		t.Fatalf("error = %v, want registry error envelope", err)
	}
}

func newCatalogTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "catalog.sqlite"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func localConfig(id string, gitURL string) ServiceContract {
	return ServiceContract{
		ServiceID: id, Name: id, RuntimeDir: "/tmp/" + id,
		HealthURL: "http://127.0.0.1:1/health",
		StartCmd:  "true", StopCmd: "true", RestartCmd: "true",
		GitRepoURL: gitURL,
	}
}

func TestBuildServiceCatalogMergesRegistryAndLocalConfig(t *testing.T) {
	store := newCatalogTestStore(t)
	if _, err := store.UpsertService(localConfig("web-cursor", "https://github.com/kaulie/agent-control-plane")); err != nil {
		t.Fatalf("UpsertService: %v", err)
	}
	if _, err := store.UpsertService(localConfig("old-only", "")); err != nil {
		t.Fatalf("UpsertService: %v", err)
	}
	srv := fakeServiceRegistry(t, []RegistryService{
		{Namespace: "default", Name: "service_registry", Version: "0.0.1"},
		{Namespace: "default", Name: "web-cursor", Version: "9.9.9", Owner: "kaulie",
			GitRepoURL: "https://github.com/kaulie/agent-control-plane"},
	})

	entries, status, err := buildServiceCatalog(context.Background(), store, registryClientFor(srv.URL))
	if err != nil {
		t.Fatalf("buildServiceCatalog: %v", err)
	}
	if !status.Enabled || !status.OK || status.Services != 2 {
		t.Fatalf("status = %+v", status)
	}
	if len(entries) != 3 {
		t.Fatalf("want 3 entries (2 registry + 1 local-only), got %d", len(entries))
	}
	byID := map[string]ServiceCatalogEntry{}
	for _, e := range entries {
		byID[e.ServiceID] = e
	}
	reg := byID["service_registry"]
	if !reg.Registered || reg.Configured || reg.Registry == nil || reg.Registry.Version != "0.0.1" {
		t.Fatalf("service_registry entry = %+v", reg)
	}
	wc := byID["web-cursor"]
	if !wc.Registered || !wc.Configured {
		t.Fatalf("web-cursor entry = %+v", wc)
	}
	if wc.RuntimeDir != "/tmp/web-cursor" || wc.Registry == nil || wc.Registry.Version != "9.9.9" {
		t.Fatalf("web-cursor merge lost data: %+v", wc)
	}
	old := byID["old-only"]
	if old.Registered || !old.Configured {
		t.Fatalf("local-only entry = %+v", old)
	}
	// Registry order first, local-only appended after.
	if entries[0].ServiceID != "service_registry" || entries[1].ServiceID != "web-cursor" || entries[2].ServiceID != "old-only" {
		t.Fatalf("unexpected order: %s, %s, %s", entries[0].ServiceID, entries[1].ServiceID, entries[2].ServiceID)
	}
}

func TestBuildServiceCatalogKeepsLocalRowsWhenRegistryDown(t *testing.T) {
	store := newCatalogTestStore(t)
	if _, err := store.UpsertService(localConfig("web-cursor", "")); err != nil {
		t.Fatalf("UpsertService: %v", err)
	}
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	entries, status, err := buildServiceCatalog(context.Background(), store, registryClientFor(deadURL))
	if err != nil {
		t.Fatalf("buildServiceCatalog: %v", err)
	}
	if !status.Enabled || status.OK || status.Error == "" {
		t.Fatalf("status = %+v, want enabled+failed+error", status)
	}
	if len(entries) != 1 || entries[0].ServiceID != "web-cursor" {
		t.Fatalf("entries = %+v", entries)
	}
	if entries[0].Registered {
		t.Fatal("registry unreachable → registered must be false (cannot verify)")
	}
	if !entries[0].Configured {
		t.Fatal("local config must survive a registry outage")
	}
}

func putServiceJSON(t *testing.T, srv *apiServer, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/services/"+id, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

func TestPutServiceOnlyConfiguresRegisteredServices(t *testing.T) {
	store := newCatalogTestStore(t)
	srv := fakeServiceRegistry(t, []RegistryService{
		{Name: "web-cursor", GitRepoURL: "https://github.com/kaulie/agent-control-plane"},
	})
	api := &apiServer{store: store, registry: registryClientFor(srv.URL)}
	body := `{"runtimeDir":"/tmp/web-cursor","healthUrl":"http://127.0.0.1:4211/health",` +
		`"startCmd":"true","stopCmd":"true","restartCmd":"true"}`

	// 1) 未在注册中心登记 → 拒绝新建。
	rec := putServiceJSON(t, api, "brand-new", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT unregistered = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "未在 service_registry 中登记") {
		t.Fatalf("error message should point at service_registry: %s", rec.Body.String())
	}

	// 2) 已登记 → 首次配置成功，gitRepoUrl 从注册中心兜底。
	rec = putServiceJSON(t, api, "web-cursor", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT registered = %d, body=%s", rec.Code, rec.Body.String())
	}
	var created ServiceContract
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.GitRepoURL != "https://github.com/kaulie/agent-control-plane" {
		t.Fatalf("gitRepoUrl = %q, want the registry's value", created.GitRepoURL)
	}

	// 3) 再次 PUT = 更新，不是新建。
	if rec = putServiceJSON(t, api, "web-cursor", body); rec.Code != http.StatusOK {
		t.Fatalf("PUT update = %d, body=%s", rec.Code, rec.Body.String())
	}

	// 4) 注册中心不可用：已有本地配置仍可编辑（不能把运维锁死）。
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	api.registry = registryClientFor(deadURL)
	if rec = putServiceJSON(t, api, "web-cursor", body); rec.Code != http.StatusOK {
		t.Fatalf("PUT existing while registry down = %d, body=%s", rec.Code, rec.Body.String())
	}
	if rec = putServiceJSON(t, api, "brand-new", body); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("PUT new while registry down = %d, want 503 (cannot verify)", rec.Code)
	}

	// 5) 注册中心未配置：本机一律不能新建。
	api.registry = nil
	if rec = putServiceJSON(t, api, "another-new", body); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("PUT new without registry = %d, want 503", rec.Code)
	}
}

func TestResolveServiceGitRepo(t *testing.T) {
	srv := fakeServiceRegistry(t, []RegistryService{
		{Name: "svc", GitRepoURL: "https://github.com/kaulie/registry-repo"},
	})
	reg := registryClientFor(srv.URL)

	// 本地显式配置优先。
	if got := resolveServiceGitRepo(context.Background(), reg,
		&ServiceContract{ServiceID: "svc", GitRepoURL: "https://github.com/kaulie/local"}); got != "https://github.com/kaulie/local" {
		t.Fatalf("local override = %q", got)
	}
	// 本地留空 → 用注册中心登记的仓库。
	if got := resolveServiceGitRepo(context.Background(), reg, &ServiceContract{ServiceID: "svc"}); got != "https://github.com/kaulie/registry-repo" {
		t.Fatalf("registry fallback = %q", got)
	}
	// 两边都没有 → 空。
	if got := resolveServiceGitRepo(context.Background(), reg, &ServiceContract{ServiceID: "other"}); got != "" {
		t.Fatalf("no repo = %q, want empty", got)
	}
	// 注册中心不可用且本地留空 → 空（调用方给明确报错）。
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	if got := resolveServiceGitRepo(context.Background(), registryClientFor(deadURL), &ServiceContract{ServiceID: "svc"}); got != "" {
		t.Fatalf("registry down = %q, want empty", got)
	}
}

func TestConfigServiceRegistryURL(t *testing.T) {
	t.Setenv("SERVICE_REGISTRY_URL", "")
	if got := loadConfig().ServiceRegistryURL; got != defaultServiceRegistryURL {
		t.Fatalf("default = %q, want %q", got, defaultServiceRegistryURL)
	}
	t.Setenv("SERVICE_REGISTRY_URL", "http://127.0.0.1:9999/")
	if got := loadConfig().ServiceRegistryURL; got != "http://127.0.0.1:9999/" {
		t.Fatalf("override = %q", got)
	}
	t.Setenv("SERVICE_REGISTRY_URL", "off")
	if got := loadConfig().ServiceRegistryURL; got != "" {
		t.Fatalf("off = %q, want empty", got)
	}
	t.Setenv("SERVICE_REGISTRY_URL", "")
	t.Setenv("SERVICE_REGISTRY_TIMEOUT_SEC", "9")
	if got := loadConfig().ServiceRegistryTimeout; got != 9*time.Second {
		t.Fatalf("timeout = %v", got)
	}
}
