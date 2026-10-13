package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// 契约里的 runtimeEnv：服务启动（startCmd / stopCmd / restartCmd）时额外注入的环境变量。
// 平台固定注入的键不在这里配，启动时由平台写入并覆盖同名键。

const (
	maxRuntimeEnvEntries  = 64
	maxRuntimeEnvKeyLen   = 128
	maxRuntimeEnvValueLen = 8192
)

// runtimeEnvKeyPattern 是 POSIX 环境变量名：字母或下划线开头，其余为字母、数字、下划线。
var runtimeEnvKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// isPlatformInjectedEnv 这些键由部署平台在启动时写入（端口、版本、机器、目录）。
// 契约里再配同名键没有意义：平台值一定会盖掉它。
func isPlatformInjectedEnv(key string) bool {
	switch key {
	case "SERVICE_PORT", "PORT", "APP_VERSION", "DEPLOY_MACHINE", "RUNTIME_DIR":
		return true
	default:
		return false
	}
}

// NormalizeRuntimeEnv 校验并归一运行时环境变量。空 map / nil → (nil, nil)。
// 键会去掉首尾空白；值原样保留（允许空字符串）。非法项返回错误，不静默丢掉。
func NormalizeRuntimeEnv(in map[string]string) (map[string]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(in))
	for rawKey, value := range in {
		key := strings.TrimSpace(rawKey)
		if key == "" {
			return nil, fmt.Errorf("运行时环境变量的名字不能为空")
		}
		if len(key) > maxRuntimeEnvKeyLen {
			return nil, fmt.Errorf("运行时环境变量 %s 的名字过长（上限 %d）", key, maxRuntimeEnvKeyLen)
		}
		if !runtimeEnvKeyPattern.MatchString(key) {
			return nil, fmt.Errorf("运行时环境变量 %s 无效：名字只能包含字母、数字和下划线，且不能以数字开头", key)
		}
		if isPlatformInjectedEnv(key) {
			return nil, fmt.Errorf("运行时环境变量 %s 由平台在启动时注入，不能在这里配置", key)
		}
		if strings.ContainsAny(value, "\r\n\x00") {
			return nil, fmt.Errorf("运行时环境变量 %s 的值不能包含换行", key)
		}
		if len(value) > maxRuntimeEnvValueLen {
			return nil, fmt.Errorf("运行时环境变量 %s 的值过长（上限 %d）", key, maxRuntimeEnvValueLen)
		}
		if _, exists := out[key]; exists {
			return nil, fmt.Errorf("运行时环境变量 %s 重复了", key)
		}
		out[key] = value
	}
	if len(out) > maxRuntimeEnvEntries {
		return nil, fmt.Errorf("运行时环境变量最多 %d 项", maxRuntimeEnvEntries)
	}
	return out, nil
}

// usableRuntimeEnv 是启动时实际注入的那一份。已保存的配置应当能通过
// NormalizeRuntimeEnv；万一库里有脏数据，丢掉非法项，不让一条坏键挡住启动。
func usableRuntimeEnv(in map[string]string) map[string]string {
	out, err := NormalizeRuntimeEnv(in)
	if err == nil {
		return out
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	kept := map[string]string{}
	for _, raw := range keys {
		norm, err := NormalizeRuntimeEnv(map[string]string{raw: in[raw]})
		if err != nil || len(norm) == 0 {
			continue
		}
		for k, v := range norm {
			if len(kept) >= maxRuntimeEnvEntries {
				return kept
			}
			kept[k] = v
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

// applyUserRuntimeEnv 把契约里的运行时环境变量写入 env。平台键由调用方在这之后写入。
func applyUserRuntimeEnv(env map[string]string, service ServiceContract) {
	for k, v := range usableRuntimeEnv(service.RuntimeEnv) {
		if isPlatformInjectedEnv(k) {
			continue
		}
		env[k] = v
	}
}

// userRuntimeEnvKeys 是启动命令里来自契约（而不是平台）的变量名，给时间线打码用。
func userRuntimeEnvKeys(service ServiceContract) map[string]bool {
	keys := map[string]bool{}
	for k := range usableRuntimeEnv(service.RuntimeEnv) {
		if isPlatformInjectedEnv(k) {
			continue
		}
		keys[k] = true
	}
	return keys
}

// runtimeEnvEventSuffix 附在部署事件后面：只列变量名，不写值（值可能是密钥）。
func runtimeEnvEventSuffix(service ServiceContract) string {
	keys := make([]string, 0)
	for k := range userRuntimeEnvKeys(service) {
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return ""
	}
	sort.Strings(keys)
	return "，运行时环境变量 " + strings.Join(keys, "、")
}

func encodeRuntimeEnv(in map[string]string) (string, error) {
	normalized, err := NormalizeRuntimeEnv(in)
	if err != nil {
		return "", err
	}
	if len(normalized) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func decodeRuntimeEnv(raw string) map[string]string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return nil
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return usableRuntimeEnv(out)
}
