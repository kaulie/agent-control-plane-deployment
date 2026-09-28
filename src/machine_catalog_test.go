package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

// 机器是**服务的字段**：下拉与触发校验都按服务收窄 —— 只有这个服务在注册中心登记在案的
// 机器（+ 本机）才出现。以前所有服务共用一份全局列表，于是「event-center 的下拉里能看到
// autonomy 的远端机器」。
func TestMachineCatalogScopeIsPerService(t *testing.T) {
	cfg := Config{
		DeployMachineTargets: map[string]MachineTarget{
			"local":    {ID: "local", Kind: "local"},
			"10.0.0.7": {ID: "10.0.0.7", Kind: "ssh", SSHUser: "ubuntu", SSHHost: "10.0.0.7", RuntimeHome: "/home/ubuntu/runtime"},
			"gpu-2":    {ID: "gpu-2", Kind: "ssh", SSHUser: "ubuntu", SSHHost: "10.0.0.8", RuntimeHome: "/home/ubuntu/runtime"},
			"staging":  {ID: "staging", Kind: "ssh", SSHUser: "ubuntu", SSHHost: "10.0.0.9", RuntimeHome: "/home/ubuntu/runtime"},
		},
		// 显式全局补充：给还没在注册中心登记实例的机器用（服务第一次上某台机器）。
		DeployMachines: []string{"staging"},
	}
	cat := NewMachineCatalog(cfg, fakeRegistry(t, snapshotWithHosts, true))

	cases := []struct{ svc, want string }{
		{"", "local,10.0.0.7,gpu-2,staging"},                // 没给服务 = 全局视图
		{"web-cursor", "local,10.0.0.7,staging"},            // 127.0.0.1 + 10.0.0.7（+ 全局补充）
		{"event-center", "local,staging"},                   // 只在本机跑
		{"autonomy", "local,gpu-2,staging"},                 // metadata.machine=gpu-2（两台实例归并成一台）
		{"unknown-service", "local,10.0.0.7,gpu-2,staging"}, // 注册中心不知道它 → 退回全局视图
	}
	for _, tc := range cases {
		ids, _, _ := cat.For(t.Context(), tc.svc)
		if got := strings.Join(ids, ","); got != tc.want {
			t.Errorf("For(%q) = %v, want %v", tc.svc, got, tc.want)
		}
	}

	// 触发校验同样按服务：别的服务的机器被拒，并说清是「那台机器上没有这个服务的实例」。
	_, err := cat.ValidateForService(t.Context(), "event-center", "gpu-2")
	if err == nil || !strings.Contains(err.Error(), "event-center") || !strings.Contains(err.Error(), "实例登记") {
		t.Fatalf("gpu-2 是 autonomy 的机器，event-center 选它必须被拒并说明原因，got %v", err)
	}
	if !strings.Contains(err.Error(), "event-center 可选：local, staging") {
		t.Fatalf("拒绝信息要附上本服务的允许列表，got %v", err)
	}
	if got, err := cat.ValidateForService(t.Context(), "autonomy", "gpu-2"); err != nil || got != "gpu-2" {
		t.Fatalf("autonomy 选自己的 gpu-2 应通过，got %q, %v", got, err)
	}
	// 全局补充（DEPLOY_MACHINES）里的机器对所有服务可选。
	if got, err := cat.ValidateForService(t.Context(), "event-center", "staging"); err != nil || got != "staging" {
		t.Fatalf("显式补充的机器应对所有服务可选，got %q, %v", got, err)
	}
	// 不选 = 默认机器（本机恒在列表最前）。
	if got, err := cat.ValidateForService(t.Context(), "event-center", ""); err != nil || got != "local" {
		t.Fatalf("空 = 默认机器 local，got %q, %v", got, err)
	}
	// 默认机器不在这份列表里时也要退回本机（不会把别的服务的机器当成它的默认）。
	cfg2 := cfg
	cfg2.DefaultDeployMachine = "gpu-2"
	cat2 := NewMachineCatalog(cfg2, fakeRegistry(t, snapshotWithHosts, true))
	if got := cat2.DefaultFor(t.Context(), "event-center"); got != "local" {
		t.Fatalf("event-center 的默认机器 = %q, want local（gpu-2 不是它的机器）", got)
	}
	if _, ok := cat2.TargetsFor(t.Context(), "event-center")["gpu-2"]; ok {
		t.Fatal("TargetsFor(event-center) 不该包含别的服务的机器")
	}
}

// 有该服务的实例、但没配通道的机器：不在列表里，但要在说明里点名（否则「明明在跑却选不到」
// 会变成新的谜题）。
func TestMachineCatalogScopeNamesChannellessMachines(t *testing.T) {
	const snapshot = `{
  "services": [{"namespace":"default","name":"web-cursor"}],
  "instances": [
    {"namespace":"default","service":"web-cursor","host":"127.0.0.1","port":4211},
    {"namespace":"default","service":"web-cursor","host":"10.0.0.5","port":4211},
    {"namespace":"default","service":"event-center","host":"127.0.0.1","port":4290}
  ]
}`
	cfg := Config{DeployMachineTargets: map[string]MachineTarget{
		"local": {ID: "local", Kind: "local"},
	}}
	cat := NewMachineCatalog(cfg, fakeRegistry(t, snapshot, true))

	ids, _, note := cat.For(t.Context(), "web-cursor")
	if strings.Join(ids, ",") != "local" {
		t.Fatalf("web-cursor 的列表 = %v, want [local]（10.0.0.5 没通道）", ids)
	}
	if !strings.Contains(note, "10.0.0.5") || !strings.Contains(note, deployMachineTargetsEnv) {
		t.Fatalf("说明要点名「有实例但没通道」的机器，got %q", note)
	}
}

// 注册中心不可达时，按服务收窄无从谈起（不知道服务跑在哪）：退回通道视图，并说明原因。
func TestMachineCatalogScopeFallsBackWhenRegistryDown(t *testing.T) {
	cfg := Config{DeployMachineTargets: map[string]MachineTarget{
		"local":    {ID: "local", Kind: "local"},
		"10.0.0.7": {ID: "10.0.0.7", Kind: "ssh", SSHHost: "10.0.0.7", RuntimeHome: "/home/ubuntu/runtime"},
	}}
	cat := NewMachineCatalog(cfg, fakeRegistry(t, "", false))

	ids, _, note := cat.For(t.Context(), "web-cursor")
	if strings.Join(ids, ",") != "local,10.0.0.7" {
		t.Fatalf("outage 时按服务收窄要退回通道列表，got %v", ids)
	}
	if !strings.Contains(note, "注册中心不可达") {
		t.Fatalf("note must explain the outage, got %q", note)
	}
	// 注册中心里没有这个服务：同样退回，但说明是「没有实例登记」。
	cat2 := NewMachineCatalog(cfg, fakeRegistry(t, snapshotWithHosts, true))
	ids2, _, note2 := cat2.For(t.Context(), "never-deployed")
	if strings.Join(ids2, ",") != "local,10.0.0.7" || !strings.Contains(note2, "never-deployed") {
		t.Fatalf("For(unknown service) = %v note = %q, want the channel list + a note naming it", ids2, note2)
	}
}

// /api/meta 的机器列表按 ?serviceId= 收窄：面板据此给每个服务列出自己的机器。
func TestMetaDeployMachinesAreScopedToService(t *testing.T) {
	srv, store := newTestIdentityServer(t, true)
	// 触发校验要认这两条契约（store 里默认只有 web-cursor）。
	for i, id := range []string{"event-center", "autonomy"} {
		if _, err := store.UpsertService(ServiceContract{
			ServiceID:  id,
			Name:       id,
			RuntimeDir: filepath.Join(t.TempDir(), id),
			HealthURL:  "/health",
			Port:       4300 + i,
			StartCmd:   "true",
			StopCmd:    "true",
			RestartCmd: "true",
			GitRepoURL: "https://github.com/kaulie/" + id,
		}); err != nil {
			t.Fatalf("UpsertService(%s): %v", id, err)
		}
	}
	srv.registry = fakeRegistry(t, snapshotWithHosts, true)
	srv.cfg.DeployMachineTargets = map[string]MachineTarget{
		"local":    {ID: "local", Kind: "local"},
		"10.0.0.7": {ID: "10.0.0.7", Kind: "ssh", SSHUser: "ubuntu", SSHHost: "10.0.0.7", RuntimeHome: "/home/ubuntu/runtime"},
		"gpu-2":    {ID: "gpu-2", Kind: "ssh", SSHUser: "ubuntu", SSHHost: "10.0.0.8", RuntimeHome: "/home/ubuntu/runtime"},
	}
	srv.machines = NewMachineCatalog(srv.cfg, srv.registry)

	machinesFor := func(query string) ([]string, string, string) {
		t.Helper()
		var meta map[string]any
		if err := json.Unmarshal(getJSON(t, srv, "/api/meta"+query).Body.Bytes(), &meta); err != nil {
			t.Fatalf("decode meta: %v", err)
		}
		raw, _ := meta["deployMachines"].([]any)
		ids := []string{}
		for _, id := range raw {
			ids = append(ids, id.(string))
		}
		if len(ids) == 0 {
			t.Fatalf("/api/meta%s 的机器列表不能是空", query)
		}
		scope, _ := meta["deployMachineScope"].(string)
		hint, _ := meta["deployMachineHint"].(string)
		return ids, scope, hint
	}

	// web-cursor 在 127.0.0.1 与 10.0.0.7 上有实例；event-center 只在本机；autonomy 在 gpu-2。
	if ids, scope, hint := machinesFor("?serviceId=web-cursor"); strings.Join(ids, ",") != "local,10.0.0.7" || scope != "web-cursor" {
		t.Fatalf("meta?serviceId=web-cursor → %v scope=%q, want [local 10.0.0.7]", ids, scope)
	} else if !strings.Contains(hint, "按**服务**取自 service_registry") {
		t.Fatalf("hint 要写明列表按服务取，got %q", hint)
	}
	if ids, _, _ := machinesFor("?serviceId=event-center"); strings.Join(ids, ",") != "local" {
		t.Fatalf("meta?serviceId=event-center → %v, want [local]（它只在本地跑）", ids)
	}
	if ids, _, _ := machinesFor("?serviceId=autonomy"); strings.Join(ids, ",") != "local,gpu-2" {
		t.Fatalf("meta?serviceId=autonomy → %v, want [local gpu-2]", ids)
	}
	// 不给 serviceId = 全局视图（兼容老调用方与元信息页）。
	if ids, scope, _ := machinesFor(""); strings.Join(ids, ",") != "local,10.0.0.7,gpu-2" || scope != "" {
		t.Fatalf("meta（全局）→ %v scope=%q, want all three", ids, scope)
	}
	// 注册中心里没有这个服务 → 退回全局，但说明必须写在最前面（别让「全部机器」看起来
	// 像「这个服务的机器」）。
	if ids, _, hint := machinesFor("?serviceId=never-registered"); strings.Join(ids, ",") != "local,10.0.0.7,gpu-2" ||
		!strings.Contains(hint, "没能按服务收窄") {
		t.Fatalf("未知服务 → 退回全局并说明原因，got %v hint=%q", ids, hint)
	}

	// 触发校验也按服务：event-center 选 autonomy 的机器 → 400（并说明那台机器上没有它）。
	rec := postJSON(t, srv, "/api/deploy-notify",
		`{"serviceId":"event-center","targetMachine":"gpu-2"}`,
		map[string]string{"identity_role": "user", "identity_id": "user_001"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("别的服务的机器必须 400，status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "event-center") {
		t.Fatalf("400 要说清是哪个服务没有那台机器的实例，got %s", rec.Body.String())
	}
	rec = postJSON(t, srv, "/api/deploy-notify",
		`{"serviceId":"autonomy","targetMachine":"gpu-2"}`,
		map[string]string{"identity_role": "user", "identity_id": "user_001"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("本服务的机器应被接受，status = %d body=%s", rec.Code, rec.Body.String())
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
