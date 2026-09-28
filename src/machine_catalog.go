package main

import (
	"context"
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
		byService: map[string][]string{}, platforms: map[string]BuildPlatform{}}
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

// For 返回**某个服务**的可部署机器：本机 + 这个服务在注册中心登记在案的机器（且必须有通道）。
//
// 收窄的理由：机器是服务的字段（服务在哪些机器上有实例，注册中心知道）。把别的服务的机器
// 列进下拉毫无意义 —— 那不是跑这个服务的地方。
//
// 三种「问不出来」的情况都退回全局视图（并在 note 里说清），因为把它们当成「只有本机」
// 会挡住合法部署（服务第一次上某台机器时，它在注册中心还没有那台机器的实例）：
//   - serviceID 为空（调用方没给服务）；
//   - 注册中心不可达 / 没配置；
//   - 注册中心里没有这个服务的实例登记。
func (c *MachineCatalog) For(ctx context.Context, serviceID string) ([]string, string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refresh(ctx)

	serviceID = strings.TrimSpace(serviceID)
	if serviceID == "" {
		return append([]string(nil), c.deployable...), c.source, c.note
	}
	svcMachines, known := c.byService[serviceID]
	if !c.registryOK {
		return append([]string(nil), c.deployable...), c.source, c.note
	}
	if !known {
		return append([]string(nil), c.deployable...), c.source,
			joinNotes("service_registry 里没有 "+serviceID+" 的实例登记，机器列表退回全部有通道的机器", c.note)
	}

	// 本机恒在（控制面自己就跑在这台机器上，也是不选机器时的默认），其余取交集：
	// 既是这个服务的机器，又有部署通道。
	inScope := map[string]bool{defaultDeployMachine: true}
	registered := append([]string(nil), svcMachines...)
	for _, id := range svcMachines {
		inScope[id] = true
	}
	// DEPLOY_MACHINES 是**显式**的全局补充（服务第一次上某台机器、注册中心还没有它的实例
	// 时用）：写进去的机器对所有服务可选。
	for _, id := range c.cfg.DeployMachines {
		inScope[strings.TrimSpace(id)] = true
	}
	ids := []string{}
	for _, id := range c.deployable {
		if inScope[id] {
			ids = append(ids, id)
		}
	}
	// 有该服务的实例、但没配通道的机器：说清它们为什么不在列表里。
	channelLess := []string{}
	for _, id := range registered {
		if !containsID(ids, id) {
			channelLess = append(channelLess, id)
		}
	}
	note := ""
	if len(channelLess) > 0 {
		sort.Strings(channelLess)
		note = joinNotes("这些机器有 "+serviceID+" 的实例但没配部署通道，暂不可选："+
			strings.Join(channelLess, ", ")+"（配 "+deployMachineTargetsEnv+" 后可部署）", c.note)
	}
	source := "registry"
	if len(ids) <= 1 {
		source = "registry-local"
	}
	return ids, source, note
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
		if t, ok := c.cfg.DeployMachineTargets[id]; ok {
			out[id] = t
		}
	}
	return out
}

// Target 返回某台可部署机器的通道。
func (c *MachineCatalog) Target(ctx context.Context, id string) (MachineTarget, bool) {
	targets := c.Targets(ctx)
	t, ok := targets[id]
	return t, ok
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
// 否则列表第一台（本机恒在最前）。
func (c *MachineCatalog) DefaultFor(ctx context.Context, serviceID string) string {
	ids, _, _ := c.For(ctx, serviceID)
	return c.defaultFrom(ids)
}

// Validate 归一化「本次选择的部署机器」（全局视图；调用方不知道服务时用）。
func (c *MachineCatalog) Validate(ctx context.Context, sel string) (string, error) {
	return c.ValidateForService(ctx, "", sel)
}

// ValidateForService 归一化某个服务的「本次选择的部署机器」：空 = 默认机器；非空必须是
// **这个服务**可选（有通道）的机器。三种拒绝各自给出可操作的下一步：
//
//   - 有通道、但不是这个服务的机器 → 说清「那台机器上没有它的实例登记」；
//   - 知道这台机器、但没配通道 → 指向 DEPLOY_MACHINE_TARGETS；
//   - 完全不认识 → 附本服务允许列表。
func (c *MachineCatalog) ValidateForService(ctx context.Context, serviceID, sel string) (string, error) {
	sel = strings.TrimSpace(sel)
	ids, _, _ := c.For(ctx, serviceID)
	if sel == "" {
		return c.defaultFrom(ids), nil
	}
	if containsID(ids, sel) {
		return sel, nil
	}
	if svc := strings.TrimSpace(serviceID); svc != "" && c.Known(ctx, sel) {
		return "", fmt.Errorf("机器 %q 上没有服务 %q 的实例登记：机器列表按服务取自 service_registry"+
			"（先在那台机器上登记它的实例；或把该机器写进 %s 作为全局补充）。%s 可选：%s",
			sel, svc, deployMachinesEnv, svc, strings.Join(ids, ", "))
	}
	if c.KnownDiscovered(ctx, sel) {
		return "", fmt.Errorf("deploy machine %q 还没有部署通道：在 %s（或 data/machine-targets）里给它配 `ssh [user@]host[:port] <remote-runtime-home>`；可选：%s",
			sel, deployMachineTargetsEnv, strings.Join(ids, ", "))
	}
	return "", fmt.Errorf("unknown deploy machine %q; allowed: %s", sel, strings.Join(ids, ", "))
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
				if svc := strings.TrimSpace(inst.Service); svc != "" && !containsID(byService[svc], m.ID) {
					byService[svc] = append(byService[svc], m.ID)
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
