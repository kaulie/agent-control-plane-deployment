package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 部署机器的真源是 service-registry 登记的服务实例（GET /v1/snapshot）。

const snapshotWithHosts = `{
  "services": [{"namespace":"default","name":"web-cursor"}],
  "instances": [
    {"namespace":"default","service":"web-cursor","host":"127.0.0.1","port":4211},
    {"namespace":"default","service":"event-center","host":"127.0.0.1","port":4290},
    {"namespace":"default","service":"web-cursor","host":"10.0.0.7","port":4211},
    {"namespace":"default","service":"autonomy","host":"10.0.0.8","port":4300,"metadata":{"machine":"gpu-2"}},
    {"namespace":"default","service":"autonomy","host":"10.0.0.8","port":4301,"metadata":{"machine":"gpu-2"}}
  ]
}`

func fakeRegistry(t *testing.T, snapshot string, ok bool) *ServiceRegistry {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/snapshot" {
			http.NotFound(w, r)
			return
		}
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"code":"boom","message":"registry down"}}`))
			return
		}
		_, _ = w.Write([]byte(snapshot))
	}))
	t.Cleanup(srv.Close)
	return &ServiceRegistry{baseURL: srv.URL, client: srv.Client()}
}

func TestRegistryDeployMachinesFromInstances(t *testing.T) {
	reg := fakeRegistry(t, snapshotWithHosts, true)
	ids, err := reg.DeployMachines(t.Context())
	if err != nil {
		t.Fatalf("DeployMachines: %v", err)
	}
	want := []string{"10.0.0.7", "gpu-2", "local"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("DeployMachines = %v, want %v（本机 → local；同名机器去重；metadata.machine 优先于 host）", ids, want)
	}
}

func TestMachineCatalogMergesRegistryLocalAndEnv(t *testing.T) {
	reg := fakeRegistry(t, snapshotWithHosts, true)
	cfg := Config{DeployMachines: []string{"gpu-2", "staging"}, DefaultDeployMachine: "staging"}
	cat := NewMachineCatalog(cfg, reg)

	ids, source, note := cat.List(t.Context())
	if strings.Join(ids, ",") != "local,gpu-2,staging,10.0.0.7" {
		t.Fatalf("ids = %v, want [local gpu-2 staging 10.0.0.7]", ids)
	}
	if source != "registry+env" || note != "" {
		t.Fatalf("source = %q note = %q, want registry+env/\"\"", source, note)
	}
	if got := cat.DefaultID(t.Context()); got != "staging" {
		t.Fatalf("default = %q, want staging（显式配置优先）", got)
	}
	// 注册中心里的机器可被选中（真源），未知机器仍然 400 的依据。
	if got, err := cat.Validate(t.Context(), "10.0.0.7"); err != nil || got != "10.0.0.7" {
		t.Fatalf("Validate(registry machine) = %q, %v", got, err)
	}
	if _, err := cat.Validate(t.Context(), "nope"); err == nil || !strings.Contains(err.Error(), "allowed") {
		t.Fatalf("unknown machine must be rejected with the allowed list, got %v", err)
	}
	if got, err := cat.Validate(t.Context(), ""); err != nil || got != "staging" {
		t.Fatalf("empty selection = default, got %q, %v", got, err)
	}
}

func TestMachineCatalogFallsBackWhenRegistryDown(t *testing.T) {
	cfg := Config{DeployMachines: []string{"gpu-2"}}

	// 注册中心一直不可达：退回本机 + DEPLOY_MACHINES，且说清原因；列表不会空。
	down := NewMachineCatalog(cfg, fakeRegistry(t, "", false))
	ids, source, note := down.List(t.Context())
	if strings.Join(ids, ",") != "local,gpu-2" || source != "env" {
		t.Fatalf("ids = %v source = %q, want [local gpu-2]/env", ids, source)
	}
	if !strings.Contains(note, "注册中心不可达") {
		t.Fatalf("note must explain the fallback, got %q", note)
	}

	// 先成功（缓存），随后注册中心挂掉：沿用上次的列表，并说明原因。
	reg := fakeRegistry(t, snapshotWithHosts, true)
	cat := NewMachineCatalog(cfg, reg)
	if _, _, note := cat.List(t.Context()); note != "" {
		t.Fatalf("healthy registry must not add a note, got %q", note)
	}
	reg.baseURL = "http://127.0.0.1:1" // 不可达
	cat.ttl = 0                        // 强制下次重新拉
	ids, _, note = cat.List(t.Context())
	if !strings.Contains(strings.Join(ids, ","), "10.0.0.7") {
		t.Fatalf("cached registry machines must survive an outage, got %v", ids)
	}
	if !strings.Contains(note, "沿用上次") {
		t.Fatalf("note must say the list is the previous one, got %q", note)
	}
}

func TestMetaDeployMachinesComeFromRegistry(t *testing.T) {
	srv, store := newTestIdentityServer(t, true)
	srv.registry = fakeRegistry(t, snapshotWithHosts, true)
	srv.machines = NewMachineCatalog(srv.cfg, srv.registry)

	var meta map[string]any
	if err := json.Unmarshal(getJSON(t, srv, "/api/meta").Body.Bytes(), &meta); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	ids, _ := meta["deployMachines"].([]any)
	joined := ""
	for _, id := range ids {
		joined += id.(string) + ","
	}
	if !strings.Contains(joined, "10.0.0.7") || meta["deployMachineSource"] != "registry" {
		t.Fatalf("deployMachines = %v source = %v, want the registry hosts", ids, meta["deployMachineSource"])
	}
	if hint, _ := meta["deployMachineHint"].(string); !strings.Contains(hint, "service-registry") {
		t.Fatalf("hint must name the data source, got %q", hint)
	}

	// 触发校验同样认注册中心里的机器。
	rec := postJSON(t, srv, "/api/deploy-notify",
		`{"serviceId":"web-cursor","targetMachine":"10.0.0.7"}`,
		map[string]string{"identity_role": "user", "identity_id": "user_001"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("registry machine must be accepted, status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = postJSON(t, srv, "/api/deploy-notify",
		`{"serviceId":"web-cursor","targetMachine":"nope"}`,
		map[string]string{"identity_role": "user", "identity_id": "user_001"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown machine must be 400, status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "allowed") {
		t.Fatalf("400 must list the allowed machines, got %s", rec.Body.String())
	}
	if _, err := store.GetPipeline("pipeline-anything"); err != nil {
		t.Fatalf("store sanity: %v", err)
	}
}
