package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// MachineCatalog 是「部署机器」的唯一入口：面板下拉、触发校验、部署执行都用它。
//
// **能不能被选中，取决于有没有部署通道**（MachineTarget，见 machine_target.go）：
//
//   - local：本机，永远在列表里、且是缺省默认（部署控制面自己就跑在这台机器上）；
//   - 配了 DEPLOY_MACHINE_TARGETS 的机器（含 `ssh ...` 远端）—— 会真的部署到那台机器；
//   - 只登记在 service-registry（实例主机）或 DEPLOY_MACHINES 里的机器：只是**发现**，
//     没有通道就不能选（否则会出现「选得中、却静默部署在本机」）。
//
// **列表按服务收窄**（For）：机器是**服务的字段** —— 服务在哪些机器上有实例，注册中心
// 知道（实例的 host / metadata.machine）。所以某个服务的下拉 = 本机 + 这个服务登记在案的
// 机器（且有通道）。把别的服务的机器列出来没有意义：那是别处跑的东西，选它只是把这次
// 部署送到一台没在跑这个服务的机器上。
//
// 注册中心不可达时：用 TTL 内的缓存；缓存也没有就退回本地配置 —— 可部署列表永远不会
// 是空的（面板下拉永远可用），并把原因写进 note。
type MachineCatalog struct {
	registry *ServiceRegistry
	cfg      Config
	ttl      time.Duration

	mu         sync.Mutex
	deployable []string
	discovered []string
	// byService 是「服务 → 它登记在案的机器」：列表按服务收窄时的数据源（来自注册中心的实例）。
	byService map[string][]string
	// serviceInstances 保留每个服务下「机器 id → 代表实例」，供按实例 host/metadata 解析部署通道。
	serviceInstances map[string]map[string]RegistryInstance
	// registryOK 记录这次刷新的注册中心**是否真的回答了**：false 时按服务收窄无从谈起
	// （不知道服务跑在哪），列表只能是通道给的。
	registryOK bool
	// platforms 是注册中心登记的「机器 → 平台」（机器的字段，控制面只同步）。
	platforms map[string]BuildPlatform
	source    string
	note      string
	fetchedAt time.Time
}

// machineCacheTTL 是注册中心机器列表的缓存时长：面板与触发校验会频繁问，别每次都出网。
const machineCacheTTL = 30 * time.Second

func NewMachineCatalog(cfg Config, registry *ServiceRegistry) *MachineCatalog {
	return &MachineCatalog{registry: registry, cfg: cfg, ttl: machineCacheTTL,
		byService: map[string][]string{}, serviceInstances: map[string]map[string]RegistryInstance{},
		platforms: map[string]BuildPlatform{}}
}

// PlatformFor 返回注册中心给这台机器登记的平台（ok=false = 注册中心没登记/不知道这台机器）。
func (c *MachineCatalog) PlatformFor(ctx context.Context, id string) (BuildPlatform, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refresh(ctx)
	p, ok := c.platforms[id]
	return p, ok
}

// List 返回**全局**可部署机器（面板下拉的选项）+ 来源 + 说明。
//
// 全局视图只在没有服务可问时用（/api/meta 不带 serviceId）；面板的下拉走 For(serviceID)。
func (c *MachineCatalog) List(ctx context.Context) ([]string, string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refresh(ctx)
	return append([]string(nil), c.deployable...), c.source, c.note
}

// MachineScope 是「某个服务的可部署机器」视图。
//
// Blocked 非空 = **不能发起部署**：这台服务在 service_registry 上没有任何实例登记
// （= 没绑定机器）。这时的选择是「明确失败并指路注册中心」，不是退回一份全局机器列表 ——
// 后者会让人以为「现在也能发」，其实发到哪台机器都没有依据。
type MachineScope struct {
	ServiceID string
	IDs       []string // 可选的机器（本机恒在最前；Blocked 时为空）
	Default   string   // 不选机器时用哪台
	Source    string   // registry / registry-local / targets / local / unbound
	Note      string   // 给人看的补充说明（可为空）
	Blocked   string   // 非空 = 不能发起部署，内容就是要给用户看的原因
}

// Scope 返回某个服务的机器视图（列表 / 默认 / 说明 / 是否被挡住）。
func (c *MachineCatalog) Scope(ctx context.Context, serviceID string) MachineScope {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refresh(ctx)

	serviceID = strings.TrimSpace(serviceID)
	out := MachineScope{
		ServiceID: serviceID,
		IDs:       append([]string(nil), c.deployable...),
		Source:    c.source,
		Note:      c.note,
	}
	out.Default = c.defaultFrom(out.IDs)
	if serviceID == "" {
		return out // 全局视图（没给服务）
	}
	if !c.registryOK {
		// 注册中心这次没回答：**不能**把「查不到」当成「没绑定」而拒绝部署 ——
		// 那会在注册中心抖一下的时候让所有服务都发不出去。退回通道列表并把原因写清楚。
		out.Note = joinNotes("注册中心不可达：无法确认 "+serviceID+" 绑定了哪些机器，机器列表退回全部有通道的机器", c.note)
		return out
	}
	svcMachines, known := c.byService[serviceID]
	if !known || len(svcMachines) == 0 {
		out.IDs = nil
		out.Source = "unbound"
		out.Default = defaultDeployMachine
		out.Blocked = "服务 " + serviceID + " 没有绑定部署实例（机器）：先在 service_registry 登记它的实例" +
			"（PUT /v1/namespaces/default/services/" + serviceID + "/instances，带上 host 与 metadata.platform），" +
			"绑好机器再发起部署。"
		out.Note = out.Blocked
		return out
	}

	// 本机恒在（控制面自己就跑在这台机器上，也是不选机器时的默认），其余列出
	// service_registry 上这个服务登记在案的全部机器（不再与 DEPLOY_MACHINE_TARGETS 求交）。
	registered := append([]string(nil), svcMachines...)
	ids := []string{defaultDeployMachine}
	seen := map[string]bool{defaultDeployMachine: true}
	for _, id := range registered {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	// 仍解析不出部署通道的机器：在说明里点名（列表里可见，触发时会 400）。
	channelLess := []string{}
	for _, id := range ids {
		if _, ok := c.targetForServiceLocked(serviceID, id); !ok {
			channelLess = append(channelLess, id)
		}
	}
	if len(channelLess) > 0 {
		sort.Strings(channelLess)
		out.Note = joinNotes("这些机器在 service_registry 有实例但还解析不出部署通道："+
			strings.Join(channelLess, ", ")+"（配 "+deployMachineTargetsEnv+" 或在实例 metadata 里登记 runtimeHome）", c.note)
	} else {
		out.Note = ""
	}
	out.IDs = ids
	out.Default = c.defaultFrom(ids)
	out.Source = "registry"
	if len(ids) <= 1 {
		out.Source = "registry-local"
	}
	return out
}

// For 是 Scope 的简写：只用 (ids, source, note) 的调用方（面板文案走 Scope）。
func (c *MachineCatalog) For(ctx context.Context, serviceID string) ([]string, string, string) {
	scope := c.Scope(ctx, serviceID)
	return scope.IDs, scope.Source, scope.Note
}

// joinNotes 拼接说明文案，跳过空的那半。
func joinNotes(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "；" + b
}

func containsID(ids []string, id string) bool {
	for _, m := range ids {
		if m == id {
			return true
		}
	}
	return false
}

// Discovered 返回「知道但没配通道」的机器（提示用，不可选）。
func (c *MachineCatalog) Discovered(ctx context.Context) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refresh(ctx)
	return append([]string(nil), c.discovered...)
}

// Targets 返回可部署机器 → 通道（面板据此标注本机/远端 ssh）。
func (c *MachineCatalog) Targets(ctx context.Context) map[string]MachineTarget {
	return c.TargetsFor(ctx, "")
}

// TargetsFor 返回**某个服务**可部署机器 → 通道（面板据此标注本机/远端 ssh 与平台）。
func (c *MachineCatalog) TargetsFor(ctx context.Context, serviceID string) map[string]MachineTarget {
	ids, _, _ := c.For(ctx, serviceID)
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]MachineTarget{}
	for _, id := range ids {
		if t, ok := c.targetForServiceLocked(serviceID, id); ok {
			out[id] = t
		}
	}
	return out
}

// Target 返回某台可部署机器的通道（全局视图：只看显式 DEPLOY_MACHINE_TARGETS）。
func (c *MachineCatalog) Target(ctx context.Context, id string) (MachineTarget, bool) {
	return c.TargetForService(ctx, "", id)
}

// TargetForService 返回某个服务上下文下一台机器的部署通道。
func (c *MachineCatalog) TargetForService(ctx context.Context, serviceID, id string) (MachineTarget, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refresh(ctx)
	return c.targetForServiceLocked(serviceID, id)
}

// Known 判断某台机器当前是否可选（全局视图）。
func (c *MachineCatalog) Known(ctx context.Context, id string) bool {
	return c.KnownFor(ctx, "", id)
}

// KnownFor 判断某台机器对某个服务是否可选。
func (c *MachineCatalog) KnownFor(ctx context.Context, serviceID, id string) bool {
	ids, _, _ := c.For(ctx, serviceID)
	return containsID(ids, id)
}

// DefaultID 是「不选机器」时要用的机器（全局视图）。
func (c *MachineCatalog) DefaultID(ctx context.Context) string {
	return c.DefaultFor(ctx, "")
}

// DefaultFor 是某个服务「不选机器」时要用的机器：显式默认（且在这个服务的列表里）优先，
// 否则列表第一台（本机恒在最前）；被挡住（没绑定实例）时是 local（只为让调用方拿到一个
// 非空值，实际不会被用来部署 —— 见 Scope.Blocked）。
func (c *MachineCatalog) DefaultFor(ctx context.Context, serviceID string) string {
	return c.Scope(ctx, serviceID).Default
}

// Validate 归一化「本次选择的部署机器」（全局视图；调用方不知道服务时用）。
func (c *MachineCatalog) Validate(ctx context.Context, sel string) (string, error) {
	return c.ValidateForService(ctx, "", sel)
}

// ValidateForService 归一化某个服务的「本次选择的部署机器」：空 = 默认机器；非空必须是
// **这个服务**可选（有通道）的机器。拒绝时给出可操作的下一步：
//
//   - 服务在注册中心没绑定实例 → 直接失败，指路注册中心（不是退回一份全局机器列表）；
//   - 有通道、但不是这个服务的机器 → 说清「那台机器上没有它的实例登记」；
//   - 知道这台机器、但没配通道 → 指向 DEPLOY_MACHINE_TARGETS；
//   - 完全不认识 → 附本服务允许列表。
func (c *MachineCatalog) ValidateForService(ctx context.Context, serviceID, sel string) (string, error) {
	scope := c.Scope(ctx, serviceID)
	if scope.Blocked != "" {
		return "", errors.New(scope.Blocked)
	}
	sel = strings.TrimSpace(sel)
	if sel == "" {
		return scope.Default, nil
	}
	if containsID(scope.IDs, sel) {
		if _, ok := c.TargetForService(ctx, serviceID, sel); !ok {
			return "", fmt.Errorf("deploy machine %q 还没有部署通道：在 %s（或 data/machine-targets）里给它配 `ssh [user@]host[:port] <remote-runtime-home>`，或在 service_registry 实例 metadata 里登记 runtimeHome；可选：%s",
				sel, deployMachineTargetsEnv, strings.Join(scope.IDs, ", "))
		}
		return sel, nil
	}
	if svc := strings.TrimSpace(serviceID); svc != "" && c.Known(ctx, sel) {
		return "", fmt.Errorf("机器 %q 上没有服务 %q 的实例登记：机器列表按服务取自 service_registry"+
			"（先在那台机器上登记它的实例）。%s 可选：%s",
			sel, svc, svc, strings.Join(scope.IDs, ", "))
	}
	if c.KnownDiscovered(ctx, sel) {
		return "", fmt.Errorf("deploy machine %q 还没有部署通道：在 %s（或 data/machine-targets）里给它配 `ssh [user@]host[:port] <remote-runtime-home>`；可选：%s",
			sel, deployMachineTargetsEnv, strings.Join(scope.IDs, ", "))
	}
	return "", fmt.Errorf("unknown deploy machine %q; allowed: %s", sel, strings.Join(scope.IDs, ", "))
}

// KnownDiscovered 判断某台机器是否「知道但没通道」。
func (c *MachineCatalog) KnownDiscovered(ctx context.Context, id string) bool {
	for _, m := range c.Discovered(ctx) {
		if m == id {
			return true
		}
	}
	return false
}

// defaultFrom：显式配置的默认机器（且可选）优先，否则列表第一台，最后兜 local。
func (c *MachineCatalog) defaultFrom(ids []string) string {
	if want := strings.TrimSpace(c.cfg.DefaultDeployMachine); want != "" {
		for _, m := range ids {
			if m == want {
				return want
			}
		}
	}
	if len(ids) > 0 {
		return ids[0]
	}
	return defaultDeployMachine
}

// refresh 在 TTL 过期时重算：可部署 = 通道（local 恒有）；发现 = 注册中心主机 ∪
// DEPLOY_MACHINES，减掉已经有通道的；同时记下「服务 → 它登记在案的机器」与每台机器的平台，
// 供列表按服务收窄（For）与打包平台解析（PlatformFor）使用（调用方持锁）。
func (c *MachineCatalog) refresh(ctx context.Context) {
	if len(c.deployable) > 0 && time.Since(c.fetchedAt) < c.ttl {
		return
	}
	c.deployable, c.source = c.targetIDs()

	// 发现：注册中心的实例主机（数据源统一在注册中心）+ DEPLOY_MACHINES 补充。
	// 顺带同步每台机器登记的平台（实例 metadata.platform）——打包时按它构建 ——
	// 以及「服务 → 机器」：面板下拉与触发校验据此按服务收窄。
	var fromRegistry []string
	var regErr error
	platforms := map[string]BuildPlatform{}
	byService := map[string][]string{}
	serviceInstances := map[string]map[string]RegistryInstance{}
	services := map[string]bool{}
	if c.registry.Enabled() {
		regServices, instances, err := c.registry.Snapshot(ctx)
		regErr = err
		if err == nil {
			for _, s := range regServices {
				if name := strings.TrimSpace(s.Name); name != "" {
					services[name] = true
				}
			}
			for _, inst := range instances {
				m := MachineFromInstance(inst)
				if m.ID == "" {
					continue
				}
				if !containsID(fromRegistry, m.ID) {
					fromRegistry = append(fromRegistry, m.ID)
				}
				if !m.Platform.IsZero() && platforms[m.ID].IsZero() {
					platforms[m.ID] = m.Platform
				}
				if svc := strings.TrimSpace(inst.Service); svc != "" {
					if !containsID(byService[svc], m.ID) {
						byService[svc] = append(byService[svc], m.ID)
					}
					if serviceInstances[svc] == nil {
						serviceInstances[svc] = map[string]RegistryInstance{}
					}
					serviceInstances[svc][m.ID] = inst
				}
			}
			for svc := range byService {
				sort.Strings(byService[svc])
			}
			// 注册中心里的服务：登记了服务但一个实例都没有 → 也让它是「已知服务」，
			// 列表就只剩本机（它确实没在别处跑）。
			for svc := range services {
				if _, ok := byService[svc]; !ok {
					byService[svc] = []string{}
				}
			}
		}
	} else {
		regErr = fmt.Errorf("SERVICE_REGISTRY_URL 未配置")
	}
	c.platforms = platforms
	c.byService = byService
	c.serviceInstances = serviceInstances
	c.registryOK = regErr == nil
	sort.Strings(fromRegistry)
	known := map[string]bool{}
	for _, id := range c.deployable {
		known[id] = true
	}
	discovered := []string{}
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || known[id] {
			return
		}
		known[id] = true
		discovered = append(discovered, id)
	}
	for _, id := range c.cfg.DeployMachines {
		add(id)
	}
	for _, id := range fromRegistry {
		add(id)
	}
	sort.Strings(discovered)
	c.discovered = discovered

	switch {
	case regErr != nil:
		c.note = "注册中心不可达（" + regErr.Error() + "），机器列表只用本地通道 + DEPLOY_MACHINES"
	case len(discovered) > 0:
		c.note = "这些机器只有登记、没有部署通道，暂不可选：" + strings.Join(discovered, ", ") +
			"（配 " + deployMachineTargetsEnv + " 后可部署）"
	default:
		c.note = ""
	}
	if c.cfg.MachineTargetsError != "" {
		if c.note != "" {
			c.note += "；"
		}
		c.note += "部署通道配置有误：" + c.cfg.MachineTargetsError
	}
	c.fetchedAt = time.Now()
}

// targetIDs 是「有通道的机器」列表：本机 local 在最前，其余按 id 排序。
func (c *MachineCatalog) targetIDs() ([]string, string) {
	ids := []string{defaultDeployMachine}
	seen := map[string]bool{defaultDeployMachine: true}
	rest := []string{}
	for id, t := range c.cfg.DeployMachineTargets {
		if t.ID == "" {
			t.ID = id
		}
		if id == defaultDeployMachine || seen[id] {
			continue
		}
		seen[id] = true
		rest = append(rest, id)
	}
	sort.Strings(rest)
	ids = append(ids, rest...)
	source := "local"
	if len(rest) > 0 {
		source = "targets"
	}
	return ids, source
}

// targetForServiceLocked 解析部署通道（调用方已持 c.mu）。
func (c *MachineCatalog) targetForServiceLocked(serviceID, id string) (MachineTarget, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return MachineTarget{}, false
	}
	if t, ok := c.cfg.DeployMachineTargets[id]; ok {
		if t.ID == "" {
			t.ID = id
		}
		return t, true
	}
	serviceID = strings.TrimSpace(serviceID)
	if serviceID == "" {
		return MachineTarget{}, false
	}
	inst, ok := c.serviceInstances[serviceID][id]
	if !ok {
		return MachineTarget{}, false
	}
	if t, ok := c.matchConfiguredTargetByHost(inst); ok {
		if t.ID == "" {
			t.ID = id
		}
		return t, true
	}
	return targetFromRegistryInstance(id, inst, c.cfg)
}

func (c *MachineCatalog) matchConfiguredTargetByHost(inst RegistryInstance) (MachineTarget, bool) {
	host := strings.ToLower(strings.TrimSpace(inst.Host))
	if host == "" {
		return MachineTarget{}, false
	}
	for id, t := range c.cfg.DeployMachineTargets {
		if !t.Remote() {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(t.SSHHost), host) {
			out := t
			if out.ID == "" {
				out.ID = id
			}
			return out, true
		}
	}
	return MachineTarget{}, false
}

func targetFromRegistryInstance(id string, inst RegistryInstance, cfg Config) (MachineTarget, bool) {
	if isLocalHost(inst.Host) {
		return MachineTarget{ID: id, Kind: "local"}, true
	}
	home := instanceRuntimeHome(inst, cfg)
	if home == "" {
		return MachineTarget{}, false
	}
	return MachineTarget{
		ID:          id,
		Kind:        "ssh",
		SSHHost:     strings.TrimSpace(inst.Host),
		SSHUser:     instanceSSHUser(inst),
		RuntimeHome: home,
	}, true
}

func instanceRuntimeHome(inst RegistryInstance, cfg Config) string {
	get := func(keys ...string) string {
		for _, k := range keys {
			for mk, mv := range inst.Metadata {
				if strings.EqualFold(strings.TrimSpace(mk), k) {
					if v := strings.TrimSpace(mv); v != "" {
						return v
					}
				}
			}
		}
		return ""
	}
	if v := get("runtimeHome", "runtime_home", "remoteRuntimeHome"); v != "" {
		return v
	}
	return strings.TrimSpace(cfg.Home)
}

func instanceSSHUser(inst RegistryInstance) string {
	for _, k := range []string{"sshUser", "ssh_user", "user"} {
		for mk, mv := range inst.Metadata {
			if strings.EqualFold(strings.TrimSpace(mk), k) {
				return strings.TrimSpace(mv)
			}
		}
	}
	return ""
}
