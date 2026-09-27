package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// MachineCatalog 是「部署机器」列表的唯一点：面板下拉、触发校验、部署执行都用它。
//
// 真源是 **service-registry**：它登记了每个服务实例跑在哪台机器上
// （GET /v1/snapshot 的 instances）—— 机器不再需要在控制面单独配一遍。
//
//   - local：本机，永远在列表里、且是缺省默认（部署控制面自己就跑在这台机器上）；
//   - 注册中心里的机器（实例 host，或实例 metadata.machine 给的名字）；
//   - DEPLOY_MACHINES：显式补充（还没登记实例的机器），由运维显式给出。
//
// 注册中心不可达时：用 TTL 内的缓存；缓存也没有就退回 local + DEPLOY_MACHINES，
// 并把原因记在 note 里 —— **列表永远不会是空的**（面板的下拉因此永远可用）。
//
// source 取值：registry（全部来自注册中心）/ registry+env（注册中心 + 显式补充）/ env / local。
type MachineCatalog struct {
	registry *ServiceRegistry
	cfg      Config
	ttl      time.Duration

	mu        sync.Mutex
	ids       []string
	source    string
	note      string
	fetchedAt time.Time
}

// machineCacheTTL 是注册中心机器列表的缓存时长：面板与触发校验会频繁问，别每次都出网。
const machineCacheTTL = 30 * time.Second

func NewMachineCatalog(cfg Config, registry *ServiceRegistry) *MachineCatalog {
	return &MachineCatalog{registry: registry, cfg: cfg, ttl: machineCacheTTL}
}

// List 返回机器列表 + 来源 + 说明（note 非空 = 注册中心没答上来，为什么退回本地配置）。
func (c *MachineCatalog) List(ctx context.Context) ([]string, string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refresh(ctx)
	return append([]string(nil), c.ids...), c.source, c.note
}

// Known 判断某台机器是否在列表里。
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

// Validate 归一化「本次选择的部署机器」：空 = 默认机器；非空必须是已知机器，
// 否则报错（附允许列表，供 API 回 400）。
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
	return "", fmt.Errorf("unknown deploy machine %q; allowed: %s", sel, strings.Join(ids, ", "))
}

// defaultFrom：显式配置的默认机器（且已知）优先，否则列表第一台，最后兜 local。
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

// refresh 在 TTL 过期时重新拉注册中心，并合并出最终列表（调用方持锁）。
func (c *MachineCatalog) refresh(ctx context.Context) {
	if len(c.ids) > 0 && time.Since(c.fetchedAt) < c.ttl {
		return
	}
	base, baseSource := c.localIDs()

	var regMachines []string
	var regErr error
	if c.registry.Enabled() {
		regMachines, regErr = c.registry.DeployMachines(ctx)
	} else {
		regErr = fmt.Errorf("SERVICE_REGISTRY_URL 未配置")
	}
	if regErr != nil {
		c.fetchedAt = time.Now()
		if len(c.ids) > 0 {
			// 上次注册中心给的机器还在缓存里：继续用，但把原因说清楚。
			c.note = "注册中心不可达（" + regErr.Error() + "），沿用上次的机器列表"
			return
		}
		c.ids, c.source = base, baseSource
		c.note = "注册中心不可达（" + regErr.Error() + "），已退回本机 + DEPLOY_MACHINES"
		return
	}

	ids := append([]string(nil), base...)
	seen := map[string]bool{}
	for _, m := range ids {
		seen[m] = true
	}
	added := 0
	for _, m := range regMachines {
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		ids = append(ids, m)
		added++
	}
	source := baseSource
	if added > 0 {
		if len(base) > 1 { // local + 显式配置
			source = "registry+env"
		} else {
			source = "registry"
		}
	}
	c.ids, c.source, c.note, c.fetchedAt = ids, source, "", time.Now()
}

// localIDs 是本地固定的那部分：本机 local 永远在最前，后面跟 DEPLOY_MACHINES
// 里显式补充的机器（去重、保序）。
func (c *MachineCatalog) localIDs() ([]string, string) {
	ids := []string{defaultDeployMachine}
	seen := map[string]bool{defaultDeployMachine: true}
	extra := 0
	for _, m := range c.cfg.DeployMachineIDs() {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		ids = append(ids, m)
		extra++
	}
	if extra > 0 {
		return ids, "env"
	}
	return ids, "local"
}
