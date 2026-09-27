package main

import (
	"fmt"
	"strings"
)

// 部署机器（deploy machine）：发起流水线时可选择本次部署落到哪台机器（目标主机 / agent）。
//
// 已知机器由 DEPLOY_MACHINES（逗号分隔的 id）配置；未选择时用
// DEPLOY_DEFAULT_MACHINE，未配置默认则用列表里的第一台。缺省（都不配）只有单台
// "local"，因此「不选机器」的行为与以前完全一致。
const defaultDeployMachine = "local"

// parseDeployMachines 解析 DEPLOY_MACHINES：逗号/空白/分号分隔，去重并保持声明顺序。
// 空值返回 nil（调用方退回单机缺省）。
func parseDeployMachines(raw string) []string {
	seen := map[string]bool{}
	var out []string
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool {
		switch r {
		case ',', ';', ' ', '\t', '\n', '\r':
			return true
		}
		return false
	}) {
		id := strings.TrimSpace(part)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// DeployMachineIDs 返回已知部署机器 id（未配置时是单机缺省）。
func (c Config) DeployMachineIDs() []string {
	if len(c.DeployMachines) == 0 {
		return []string{defaultDeployMachine}
	}
	return c.DeployMachines
}

// IsDeployMachine 判断 id 是否为已知机器。
func (c Config) IsDeployMachine(id string) bool {
	for _, m := range c.DeployMachineIDs() {
		if m == id {
			return true
		}
	}
	return false
}

// DefaultDeployMachineID 返回「未选择机器」时要用的机器：显式配置的默认机器，
// 否则列表第一台。
func (c Config) DefaultDeployMachineID() string {
	if strings.TrimSpace(c.DefaultDeployMachine) != "" {
		return strings.TrimSpace(c.DefaultDeployMachine)
	}
	return c.DeployMachineIDs()[0]
}

// ValidateDeployMachine 归一化「本次选择的部署机器」：空 = 默认机器；非空必须是已知
// 机器，否则返回错误（附上允许列表，供 API 回 400）。
func (c Config) ValidateDeployMachine(sel string) (string, error) {
	sel = strings.TrimSpace(sel)
	if sel == "" {
		return c.DefaultDeployMachineID(), nil
	}
	if !c.IsDeployMachine(sel) {
		return "", fmt.Errorf("unknown deploy machine %q; allowed: %s",
			sel, strings.Join(c.DeployMachineIDs(), ", "))
	}
	return sel, nil
}
