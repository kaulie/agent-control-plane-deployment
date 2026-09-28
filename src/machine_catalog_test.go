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

func TestMachineCatalogDeployableNeedsChannel(t *testing.T) {
	reg := fakeRegistry(t, snapshotWithHosts, true)
	cfg := Config{
		DeployMachines:       []string{"gpu-2", "staging"},
		DefaultDeployMachine: "staging",
		// 只有配了通道的机器才能被选中；"10.0.0.7" 是注册中心发现的主机，"staging" 是
		// 本地补充的机器 —— 都没通道。
		DeployMachineTargets: map[string]MachineTarget{
			"local":    {ID: "local", Kind: "local"},
			"gpu-2":    {ID: "gpu-2", Kind: "ssh", SSHUser: "ubuntu", SSHHost: "10.0.0.8", RuntimeHome: "/home/ubuntu/runtime"},
			"10.0.0.7": {ID: "10.0.0.7", Kind: "ssh", SSHUser: "ubuntu", SSHHost: "10.0.0.7", RuntimeHome: "/home/ubuntu/runtime"},
		},
	}
	cat := NewMachineCatalog(cfg, reg)

	ids, source, note := cat.List(t.Context())
	if strings.Join(ids, ",") != "local,10.0.0.7,gpu-2" {
		t.Fatalf("deployable ids = %v, want [local 10.0.0.7 gpu-2]（有通道的才可选）", ids)
	}
	if source != "targets" {
		t.Fatalf("source = %q, want targets", source)
	}
	if !strings.Contains(note, "staging") {
		t.Fatalf("note must name the discovered-but-channel-less machines, got %q", note)
	}
	if got := strings.Join(cat.Discovered(t.Context()), ","); got != "staging" {
		t.Fatalf("discovered = %q, want staging（gpu-2/10.0.0.7 有通道，不算发现）", got)
	}
	if got := cat.DefaultID(t.Context()); got != "local" {
		t.Fatalf("default = %q, want local（staging 配成默认但没通道 → 退回 local）", got)
	}
	if got, err := cat.Validate(t.Context(), "10.0.0.7"); err != nil || got != "10.0.0.7" {
		t.Fatalf("Validate(machine with a channel) = %q, %v", got, err)
	}
	if _, err := cat.Validate(t.Context(), "staging"); err == nil || !strings.Contains(err.Error(), "部署通道") {
		t.Fatalf("a known machine without a channel must be rejected with the channel hint, got %v", err)
	}
	if _, err := cat.Validate(t.Context(), "nope"); err == nil || !strings.Contains(err.Error(), "allowed") {
		t.Fatalf("unknown machine must be rejected with the allowed list, got %v", err)
	}
	if got, err := cat.Validate(t.Context(), ""); err != nil || got != "local" {
		t.Fatalf("empty selection = default, got %q, %v", got, err)
	}
	if t2, ok := cat.Target(t.Context(), "gpu-2"); !ok || !t2.Remote() || t2.RuntimeDirFor("web-cursor") != "/home/ubuntu/runtime/web-cursor" {
		t.Fatalf("Target(gpu-2) = %+v ok=%v, want an ssh channel with a per-service remote dir", t2, ok)
	}
}

func TestMachineCatalogFallsBackWhenRegistryDown(t *testing.T) {
	cfg := Config{
		DeployMachines: []string{"gpu-2"},
		// 可部署性由通道决定：注册中心挂不挂，gpu-2 都还能被选中。
		DeployMachineTargets: map[string]MachineTarget{
			"local": {ID: "local", Kind: "local"},
			"gpu-2": {ID: "gpu-2", Kind: "ssh", SSHHost: "10.0.0.8", RuntimeHome: "/home/ubuntu/runtime"},
		},
	}

	// 注册中心一直不可达：机器列表仍由通道给出（不会空），note 说清原因。
	down := NewMachineCatalog(cfg, fakeRegistry(t, "", false))
	ids, source, note := down.List(t.Context())
	if strings.Join(ids, ",") != "local,gpu-2" || source != "targets" {
		t.Fatalf("ids = %v source = %q, want [local gpu-2]/targets", ids, source)
	}
	if !strings.Contains(note, "注册中心不可达") {
		t.Fatalf("note must explain the fallback, got %q", note)
	}

	// 注册中心不可达时：可部署列表仍然只有本机（部署通道决定的），note 说明原因。
	reg := fakeRegistry(t, snapshotWithHosts, true)
	cat := NewMachineCatalog(cfg, reg)
	if ids, _, note := cat.List(t.Context()); strings.Join(ids, ",") != "local,gpu-2" || note == "" {
		t.Fatalf("healthy registry: ids=%v note=%q（gpu-2 是本地通道，应可选）", ids, note)
	}
	reg.baseURL = "http://127.0.0.1:1" // 不可达
	cat.ttl = 0                        // 强制下次重算
	ids, _, note = cat.List(t.Context())
	if strings.Join(ids, ",") != "local,gpu-2" {
		t.Fatalf("an outage must not drop the machines that have a channel, got %v", ids)
	}
	if !strings.Contains(note, "注册中心不可达") {
		t.Fatalf("note must explain the outage, got %q", note)
	}
}

func TestMetaDeployMachinesComeFromRegistry(t *testing.T) {
	srv, store := newTestIdentityServer(t, true)
	srv.registry = fakeRegistry(t, snapshotWithHosts, true)
	// 注册中心发现了 10.0.0.7；给它配一条 ssh 通道后它才可选、才会真的部署过去。
	srv.cfg.DeployMachineTargets = map[string]MachineTarget{
		"local":    {ID: "local", Kind: "local"},
		"10.0.0.7": {ID: "10.0.0.7", Kind: "ssh", SSHUser: "ubuntu", SSHHost: "10.0.0.7", RuntimeHome: "/home/ubuntu/runtime"},
	}
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
	if !strings.Contains(joined, "10.0.0.7") || meta["deployMachineSource"] != "targets" {
		t.Fatalf("deployMachines = %v source = %v, want the channelled machine", ids, meta["deployMachineSource"])
	}
	// 注册中心里发现但没配通道的机器（gpu-2 有 metadata.machine 名字）进 discovered。
	disc, _ := meta["deployMachineDiscovered"].([]any)
	if len(disc) != 1 || disc[0] != "gpu-2" {
		t.Fatalf("deployMachineDiscovered = %v, want [gpu-2]（发现但没通道）", meta["deployMachineDiscovered"])
	}
	if hint, _ := meta["deployMachineHint"].(string); !strings.Contains(hint, deployMachineTargetsEnv) {
		t.Fatalf("hint must name the channel config, got %q", hint)
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
