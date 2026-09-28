package main

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// 服务契约里的 URL 一律**只存路径**（例如 /health、/api/ops/restart-notify），host+port
// 在真正部署时按目标机器拼：
//
//	本机部署 → http://127.0.0.1:<port><path>（在控制面这台机器上请求）
//	远端部署 → 同一个字符串，但在**那台机器上**执行（ssh + curl 问它自己的 127.0.0.1）
//
// 于是同一份契约与「哪台机器」无关，可以原样部署到任何机器；端口只存一处（contract.port）。
//
// 兼容：历史契约里存的是完整 URL（http://127.0.0.1:4211/health）—— 保存/迁移时会把
// loopback 的 host+port 拆出去（端口落进 port，字段只留路径）；指向**外部**主机或 https
// 的 URL 视为「显式外部端点」，原样保留、不做拼接。

// NormalizeServiceURL 把一个 URL 字段归一成「存的形态」+ 推导出的端口。
//
//	/health                              → (/health, 0)
//	http://127.0.0.1:4211/health         → (/health, 4211)      // loopback = 机器相关，拆掉
//	https://127.0.0.1:4211/health        → (原样, 0)             // 自己终结 TLS，不动它
//	https://api.example.com/health       → (原样, 0)             // 外部端点，不动它
func NormalizeServiceURL(raw string) (stored string, derivedPort int) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", 0
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		// 已经是路径（或写成了别的怪东西）：原样保留，拼的时候当路径用。
		return ensureLeadingSlash(raw), 0
	}
	if u.Scheme != "http" || !isLocalHost(u.Hostname()) {
		return raw, 0 // 外部 / https：显式端点，保持原样
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	port := 0
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n > 0 && n <= 65535 {
			port = n
		}
	}
	return path, port
}

func ensureLeadingSlash(s string) string {
	if !strings.HasPrefix(s, "/") {
		return "/" + s
	}
	return s
}

// ComposeServiceURL 用某个端口的 host:port 把存的路径拼成真正要请求的 URL。
// 存的本来就是完整 URL（外部端点）时原样返回；端口未知时也原样返回（调用方会告警）。
func ComposeServiceURL(stored string, port int) string {
	stored = strings.TrimSpace(stored)
	if stored == "" {
		return ""
	}
	if u, err := url.Parse(stored); err == nil && u.Scheme != "" && u.Host != "" {
		return stored
	}
	if normalizePort(port) == 0 {
		return stored
	}
	return fmt.Sprintf("http://127.0.0.1:%d%s", normalizePort(port), ensureLeadingSlash(stored))
}

// NormalizeServiceContract 就地归一契约里的三个 URL 字段（只拆 URL，**不**动端口）：
// 新写入要求显式给出 port（API 层校验），所以这里不替调用方推导。
func NormalizeServiceContract(c *ServiceContract) bool {
	if c == nil {
		return false
	}
	changed := false
	for _, f := range []*string{&c.HealthURL, &c.RestartNotifyURL, &c.RestartPollURL} {
		stored, _ := NormalizeServiceURL(*f)
		if stored != strings.TrimSpace(*f) {
			*f = stored
			changed = true
		}
	}
	return changed
}

// adoptURLPorts 把 URL 里带的端口落进 c.Port —— 只用于**老契约迁移**：那时端口还没
// 独立成字段，只在 URL 里。新写入仍必须显式给 port（否则 400）。
func adoptURLPorts(c *ServiceContract) bool {
	if c == nil || normalizePort(c.Port) > 0 {
		return false
	}
	for _, f := range []string{c.HealthURL, c.RestartNotifyURL, c.RestartPollURL} {
		if _, port := NormalizeServiceURL(f); port > 0 {
			c.Port = port
			return true
		}
	}
	return false
}

// servicePortNum 是契约声明的服务端口（数字）；老契约没有 port 时按 healthUrl 推导。
func servicePortNum(service ServiceContract) int {
	if p := normalizePort(service.Port); p > 0 {
		return p
	}
	if n, err := strconv.Atoi(portFromHealthURL(service.HealthURL)); err == nil {
		return n
	}
	return 0
}

// serviceHealthURL / serviceNotifyURL / servicePollURL：部署时按目标机器拼出来的真地址。
func serviceHealthURL(s ServiceContract) string {
	return ComposeServiceURL(s.HealthURL, servicePortNum(s))
}

func serviceNotifyURL(s ServiceContract) string {
	return ComposeServiceURL(s.RestartNotifyURL, servicePortNum(s))
}

func servicePollURL(s ServiceContract) string {
	return ComposeServiceURL(s.RestartPollURL, servicePortNum(s))
}
