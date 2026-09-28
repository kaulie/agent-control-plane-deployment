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
// 注册中心不可达时：用 TTL 内的缓存；缓存也没有就退回本地配置 —— 可部署列表永远不会
// 是空的（面板下拉永远可用），并把原因写进 note。
type MachineCatalog struct {
	registry *ServiceRegistry
	cfg      Config
	ttl      time.Duration

	mu         sync.Mutex
	deployable []string
	discovered []string
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

// List 返回**可部署**的机器（面板下拉的选项）+ 来源 + 说明。
func (c *MachineCatalog) List(ctx context.Context) ([]string, string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refresh(ctx)
	return append([]string(nil), c.deployable...), c.source, c.note
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
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refresh(ctx)
	out := map[string]MachineTarget{}
	for _, id := range c.deployable {
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

// Known 判断某台机器当前是否可选（可部署）。
func (c *MachineCatalog) Known(ctx context.Context, id string) bool {
	ids, _, _ := c.List(ctx)
	for _, m := range ids {
		if m == id {
			return true
		}
	}
	return false
}

// DefaultID 是「不选机器」时要用的机器。
func (c *MachineCatalog) DefaultID(ctx context.Context) string {
	ids, _, _ := c.List(ctx)
	return c.defaultFrom(ids)
}

// Validate 归一化「本次选择的部署机器」：空 = 默认机器；非空必须是**有通道**的机器，
// 否则报错：已知但没通道 → 告诉对方去配通道；完全不认识 → 附允许列表。
func (c *MachineCatalog) Validate(ctx context.Context, sel string) (string, error) {
	sel = strings.TrimSpace(sel)
	ids, _, _ := c.List(ctx)
	if sel == "" {
		return c.defaultFrom(ids), nil
	}
	for _, m := range ids {
		if m == sel {
			return sel, nil
		}
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
// DEPLOY_MACHINES，减掉已经有通道的（调用方持锁）。
func (c *MachineCatalog) refresh(ctx context.Context) {
	if len(c.deployable) > 0 && time.Since(c.fetchedAt) < c.ttl {
		return
	}
	c.deployable, c.source = c.targetIDs()

	// 发现：注册中心的实例主机（数据源统一在注册中心）+ DEPLOY_MACHINES 补充。
	// 顺带同步每台机器登记的平台（实例 metadata.platform）——打包时按它构建。
	var fromRegistry []string
	var regErr error
	platforms := map[string]BuildPlatform{}
	if c.registry.Enabled() {
		var machines []RegistryMachine
		machines, regErr = c.registry.SnapshotMachines(ctx)
		for _, m := range machines {
			fromRegistry = append(fromRegistry, m.ID)
			if !m.Platform.IsZero() {
				platforms[m.ID] = m.Platform
			}
		}
	} else {
		regErr = fmt.Errorf("SERVICE_REGISTRY_URL 未配置")
	}
	c.platforms = platforms
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
