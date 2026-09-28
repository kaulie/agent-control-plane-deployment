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
		{"", "local,10.0.0.7,gpu-2,staging"}, // 没给服务 = 全局视图（含 DEPLOY_MACHINES 补充）
		{"web-cursor", "local,10.0.0.7"},     // 127.0.0.1 + 10.0.0.7
		{"event-center", "local"},            // 只在本机跑
		{"autonomy", "local,gpu-2"},          // metadata.machine=gpu-2（两台实例归并成一台）
	}
	for _, tc := range cases {
		ids, _, _ := cat.For(t.Context(), tc.svc)
		if got := strings.Join(ids, ","); got != tc.want {
			t.Errorf("For(%q) = %v, want %v", tc.svc, got, tc.want)
		}
	}

	// 注册中心里没有这个服务 = **没绑定部署实例**：直接失败、指路注册中心，
	// 不给一份「全部机器」的列表（那会让人以为现在也能发）。
	ids, source, note := cat.For(t.Context(), "unknown-service")
	if len(ids) != 0 || source != "unbound" {
		t.Fatalf("未绑定的服务 → ids=%v source=%q, want 空列表 + unbound", ids, source)
	}
	if !strings.Contains(note, "没有绑定部署实例") || !strings.Contains(note, "service_registry") {
		t.Fatalf("说明要指路注册中心，got %q", note)
	}
	if _, err := cat.ValidateForService(t.Context(), "unknown-service", ""); err == nil ||
		!strings.Contains(err.Error(), "没有绑定部署实例") {
		t.Fatalf("未绑定的服务即使不选机器也要被拒（默认机器也没有依据），got %v", err)
	}

	// 触发校验同样按服务：别的服务的机器被拒，并说清是「那台机器上没有这个服务的实例」。
	_, err := cat.ValidateForService(t.Context(), "event-center", "gpu-2")
	if err == nil || !strings.Contains(err.Error(), "event-center") || !strings.Contains(err.Error(), "实例登记") {
		t.Fatalf("gpu-2 是 autonomy 的机器，event-center 选它必须被拒并说明原因，got %v", err)
	}
	if !strings.Contains(err.Error(), "event-center 可选：local") {
		t.Fatalf("拒绝信息要附上本服务的允许列表，got %v", err)
	}
	if got, err := cat.ValidateForService(t.Context(), "autonomy", "gpu-2"); err != nil || got != "gpu-2" {
		t.Fatalf("autonomy 选自己的 gpu-2 应通过，got %q, %v", got, err)
	}
	// DEPLOY_MACHINES 是**全局视图**的补充，不再是「对所有服务都可选」：它不能绕开绑定。
	if _, err := cat.ValidateForService(t.Context(), "event-center", "staging"); err == nil ||
		!strings.Contains(err.Error(), "实例登记") {
		t.Fatalf("DEPLOY_MACHINES 里的机器不能绕开「这个服务绑没绑」的校验，got %v", err)
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

// 注册中心不可达时，按服务收窄无从谈起（不知道服务绑了哪些机器）：退回通道视图并说明原因。
// 注意这与「注册中心回答了、但没这个服务」不同 —— 后者是**没绑定**，直接挡住。
func TestMachineCatalogScopeFallsBackWhenRegistryDown(t *testing.T) {
	cfg := Config{DeployMachineTargets: map[string]MachineTarget{
		"local":    {ID: "local", Kind: "local"},
		"10.0.0.7": {ID: "10.0.0.7", Kind: "ssh", SSHHost: "10.0.0.7", RuntimeHome: "/home/ubuntu/runtime"},
	}}
	cat := NewMachineCatalog(cfg, fakeRegistry(t, "", false))

	scope := cat.Scope(t.Context(), "web-cursor")
	if strings.Join(scope.IDs, ",") != "local,10.0.0.7" || scope.Blocked != "" {
		t.Fatalf("outage 时要退回通道列表且**不能**误判成未绑定，got %+v", scope)
	}
	if !strings.Contains(scope.Note, "注册中心不可达") {
		t.Fatalf("note must explain the outage, got %q", scope.Note)
	}
	if got, err := cat.ValidateForService(t.Context(), "web-cursor", "10.0.0.7"); err != nil || got != "10.0.0.7" {
		t.Fatalf("outage 时不该阻塞部署（注册中心抖动一次就让所有服务发不出去更糟），got %q, %v", got, err)
	}

	// 注册中心回答了、但没有这个服务 → 挡住（这是「没绑定」，不是「不知道」）。
	cat2 := NewMachineCatalog(cfg, fakeRegistry(t, snapshotWithHosts, true))
	scope2 := cat2.Scope(t.Context(), "never-deployed")
	if len(scope2.IDs) != 0 || scope2.Blocked == "" || !strings.Contains(scope2.Blocked, "never-deployed") {
		t.Fatalf("未绑定的服务要被挡住并点名，got %+v", scope2)
	}
}

// /api/meta 的机器列表按 ?serviceId= 收窄：面板据此给每个服务列出自己的机器。
func TestMetaDeployMachinesAreScopedToService(t *testing.T) {
	srv, store := newTestIdentityServer(t, true)
	// 触发校验要认这几条契约（store 里默认只有 web-cursor）；unbound-svc 故意不在注册中心，
	// 用来验证「没绑定部署实例 → 直接失败」。
	for i, id := range []string{"event-center", "autonomy", "unbound-svc"} {
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

	machinesFor := func(query string) ([]string, string, string, string) {
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
		scope, _ := meta["deployMachineScope"].(string)
		hint, _ := meta["deployMachineHint"].(string)
		blocked, _ := meta["deployMachineBlocked"].(string)
		return ids, scope, hint, blocked
	}

	// web-cursor 在 127.0.0.1 与 10.0.0.7 上有实例；event-center 只在本机；autonomy 在 gpu-2。
	if ids, scope, hint, blocked := machinesFor("?serviceId=web-cursor"); strings.Join(ids, ",") != "local,10.0.0.7" ||
		scope != "web-cursor" || blocked != "" {
		t.Fatalf("meta?serviceId=web-cursor → %v scope=%q blocked=%q, want [local 10.0.0.7]", ids, scope, blocked)
	} else if !strings.Contains(hint, "按**服务**取自 service_registry") {
		t.Fatalf("hint 要写明列表按服务取，got %q", hint)
	}
	if ids, _, _, _ := machinesFor("?serviceId=event-center"); strings.Join(ids, ",") != "local" {
		t.Fatalf("meta?serviceId=event-center → %v, want [local]（它只在本地跑）", ids)
	}
	if ids, _, _, _ := machinesFor("?serviceId=autonomy"); strings.Join(ids, ",") != "local,gpu-2" {
		t.Fatalf("meta?serviceId=autonomy → %v, want [local gpu-2]", ids)
	}
	// 不给 serviceId = 全局视图（兼容老调用方与元信息页）。
	if ids, scope, _, blocked := machinesFor(""); strings.Join(ids, ",") != "local,10.0.0.7,gpu-2" || scope != "" || blocked != "" {
		t.Fatalf("meta（全局）→ %v scope=%q blocked=%q, want all three", ids, scope, blocked)
	}
	// 没绑定部署实例的服务：列表空 + 明确挡住（面板据此禁用触发按钮），提示指路注册中心。
	ids, source, hint, blocked := machinesFor("?serviceId=unbound-svc")
	if len(ids) != 0 || source != "unbound-svc" {
		t.Fatalf("未绑定的服务 → %v scope=%q, want 空列表", ids, source)
	}
	if !strings.Contains(blocked, "没有绑定部署实例") || !strings.Contains(blocked, "service_registry") {
		t.Fatalf("deployMachineBlocked 要说清原因并指路注册中心，got %q", blocked)
	}
	if !strings.Contains(hint, "没有绑定部署实例") {
		t.Fatalf("hint 要是同一句话（面板直接显示它），got %q", hint)
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
	// 未绑定的服务：不选机器也 400（「先去注册中心绑定」）。
	rec = postJSON(t, srv, "/api/deploy-notify",
		`{"serviceId":"unbound-svc"}`,
		map[string]string{"identity_role": "user", "identity_id": "user_001"})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "没有绑定部署实例") {
		t.Fatalf("未绑定的服务必须 400 并提示去注册中心绑定，status = %d body=%s", rec.Code, rec.Body.String())
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
