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
		// Port 故意不设（0 = 未指定，老数据长这样）：端口唯一索引不约束它；
		// 需要检查端口的用例自己显式给（PUT 时端口必填）。
		StartCmd: "true", StopCmd: "true", RestartCmd: "true",
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
		`"port":4211,"startCmd":"true","stopCmd":"true","restartCmd":"true"}`

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

// gitRepoUrl 是 service_registry 同步过来的元信息：本机不能改（显式改动报错），
// 且已登记的值会被同步覆盖成本机的镜像。
func TestPutServiceGitRepoURLIsRegistryOwned(t *testing.T) {
	store := newCatalogTestStore(t)
	srv := fakeServiceRegistry(t, []RegistryService{
		{Name: "web-cursor", GitRepoURL: "https://github.com/kaulie/registry-repo"},
	})
	api := &apiServer{store: store, registry: registryClientFor(srv.URL)}
	body := `{"runtimeDir":"/tmp/web-cursor","healthUrl":"http://127.0.0.1:4211/health",` +
		`"port":4211,"startCmd":"true","stopCmd":"true","restartCmd":"true"}`

	// 1) 显式改成别的值 → 400（"以为改了其实没改"更糟）。
	rec := putServiceJSON(t, api, "web-cursor",
		`{"runtimeDir":"/tmp/web-cursor","healthUrl":"http://127.0.0.1:1/health",`+
			`"port":4211,"startCmd":"true","stopCmd":"true","restartCmd":"true",`+
			`"gitRepoUrl":"https://github.com/kaulie/hacked"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT with a changed gitRepoUrl = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "本机不能修改") {
		t.Fatalf("error should explain gitRepoUrl is registry-owned: %s", rec.Body.String())
	}

	// 2) 带上注册中心登记值（幂等）→ 成功。
	rec = putServiceJSON(t, api, "web-cursor", body[:len(body)-1]+
		`,"gitRepoUrl":"https://github.com/kaulie/registry-repo"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT with the registry value = %d, body=%s", rec.Code, rec.Body.String())
	}
	// 3) 不带 gitRepoUrl → 用注册中心登记值。
	rec = putServiceJSON(t, api, "web-cursor", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT without gitRepoUrl = %d, body=%s", rec.Code, rec.Body.String())
	}
	var stored ServiceContract
	if err := json.Unmarshal(rec.Body.Bytes(), &stored); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if stored.GitRepoURL != "https://github.com/kaulie/registry-repo" {
		t.Fatalf("stored gitRepoUrl = %q, want the registry value", stored.GitRepoURL)
	}

	// 4) 注册中心登记值变化 → 本机镜像被同步覆盖（旧值不能留着）。
	srv2 := fakeServiceRegistry(t, []RegistryService{
		{Name: "web-cursor", GitRepoURL: "https://github.com/kaulie/moved-repo"},
	})
	api.registry = registryClientFor(srv2.URL)
	rec = putServiceJSON(t, api, "web-cursor", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT after registry change = %d, body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &stored); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if stored.GitRepoURL != "https://github.com/kaulie/moved-repo" {
		t.Fatalf("stored gitRepoUrl = %q, want the new registry value", stored.GitRepoURL)
	}
}

// 未登记（只有本机配置 / 注册中心没登记 gitRepoUrl）时，本机镜像的旧值既不能改
// 也不会被清空：它是这条服务唯一可用的仓库地址。
func TestPutServiceGitRepoURLStaysForUnregisteredService(t *testing.T) {
	store := newCatalogTestStore(t)
	if _, err := store.UpsertService(localConfig("legacy-only", "https://github.com/kaulie/legacy")); err != nil {
		t.Fatalf("UpsertService: %v", err)
	}
	// 注册中心返回空目录（该服务未登记）→ 已有本地配置仍可编辑。
	registryEmpty := fakeServiceRegistry(t, nil)
	api := &apiServer{store: store, registry: registryClientFor(registryEmpty.URL)}
	body := `{"runtimeDir":"/tmp/legacy-only","healthUrl":"http://127.0.0.1:1/health",` +
		`"port":4211,"startCmd":"true","stopCmd":"true","restartCmd":"true"}`

	rec := putServiceJSON(t, api, "legacy-only", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT existing local-only = %d, body=%s", rec.Code, rec.Body.String())
	}
	var stored ServiceContract
	if err := json.Unmarshal(rec.Body.Bytes(), &stored); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if stored.GitRepoURL != "https://github.com/kaulie/legacy" {
		t.Fatalf("gitRepoUrl = %q, want the kept local mirror", stored.GitRepoURL)
	}

	// 试图改成别的值 / 清空 → 都拒绝。
	if rec = putServiceJSON(t, api, "legacy-only", body[:len(body)-1]+
		`,"gitRepoUrl":"https://github.com/kaulie/other"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT changing local gitRepoUrl = %d, want 400", rec.Code)
	}
	if rec = putServiceJSON(t, api, "legacy-only", body[:len(body)-1]+
		`,"gitRepoUrl":""}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT clearing local gitRepoUrl = %d, want 400", rec.Code)
	}
}

func TestResolveServiceGitRepo(t *testing.T) {
	srv := fakeServiceRegistry(t, []RegistryService{
		{Name: "svc", GitRepoURL: "https://github.com/kaulie/registry-repo"},
	})
	reg := registryClientFor(srv.URL)

	// 注册中心登记的仓库是真源：本机镜像的旧值不能覆盖它。
	if got := resolveServiceGitRepo(context.Background(), reg,
		&ServiceContract{ServiceID: "svc", GitRepoURL: "https://github.com/kaulie/local"}); got != "https://github.com/kaulie/registry-repo" {
		t.Fatalf("registry must win = %q", got)
	}
	// 注册中心没登记该服务 → 退回本机镜像的旧值（旧数据仍可部署）。
	if got := resolveServiceGitRepo(context.Background(), reg, &ServiceContract{ServiceID: "other", GitRepoURL: "https://github.com/kaulie/local"}); got != "https://github.com/kaulie/local" {
		t.Fatalf("local fallback = %q", got)
	}
	// 两边都没有 → 空。
	if got := resolveServiceGitRepo(context.Background(), reg, &ServiceContract{ServiceID: "other"}); got != "" {
		t.Fatalf("no repo = %q, want empty", got)
	}
	// 注册中心不可用 → 退回本机镜像（不阻塞已有配置的部署）。
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	if got := resolveServiceGitRepo(context.Background(), registryClientFor(deadURL),
		&ServiceContract{ServiceID: "svc", GitRepoURL: "https://github.com/kaulie/local"}); got != "https://github.com/kaulie/local" {
		t.Fatalf("registry down = %q, want the local mirror", got)
	}
	if got := resolveServiceGitRepo(context.Background(), registryClientFor(deadURL), &ServiceContract{ServiceID: "svc"}); got != "" {
		t.Fatalf("registry down + no local = %q, want empty", got)
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
