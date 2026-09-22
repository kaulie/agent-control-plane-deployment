package main

import (
	"os"
	"sort"
	"strings"
)

// 本机代理（per-request opt-in）。
//
// 打包要在本机 `git fetch` + 跑 `build.sh`，而 GitHub 直连在有些网络里会失败
// （典型：`Error in the HTTP2 framing layer` / 超时）。本机通常已经配了代理
// （`<home>/data/proxy.env`，运维脚本同一份），但把整台服务都塞进代理并不总是
// 想要的行为，所以做成**发起流水线时的选项**：勾了才用它。
//
// 只有打包命令（git / build.sh 这类子进程）能按请求改 env；制品下载走进程内的
// HTTP client，只认进程启动时的 env（见 README「打包走本机代理」）。

// proxyEnvName 判断某个环境变量名是不是我们要接管的代理变量（大小写都认，
// 因为 git/curl 对 http_proxy 与 HTTP_PROXY 的优先级不同）。
func proxyEnvName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "http_proxy", "https_proxy", "all_proxy", "no_proxy":
		return true
	}
	return false
}

// ignoresProxy 判断名字是不是「真的指向一个代理」（只有 NO_PROXY 不算配置）。
func ignoresProxy(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), "no_proxy")
}

// parseProxyEnvFile 解析 shell 风格的环境变量文件（`export K=V`、注释、可选引号），
// 只收代理相关变量 —— `<home>/data/proxy.env` 长这样：
//
//	# Routes github.com access through the local proxy
//	export HTTPS_PROXY=http://127.0.0.1:7897
//	NO_PROXY=localhost,127.0.0.1,::1
func parseProxyEnvFile(content string) map[string]string {
	out := map[string]string{}
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
		i := strings.IndexByte(line, '=')
		if i <= 0 {
			continue
		}
		name := strings.TrimSpace(line[:i])
		if !proxyEnvName(name) {
			continue
		}
		val := strings.Trim(strings.TrimSpace(line[i+1:]), `"'`)
		if val == "" {
			continue
		}
		out[name] = val
	}
	return out
}

// envProxyVars 取出当前进程环境里的代理变量（服务自己是带着代理跑的时候用）。
func envProxyVars(env []string) map[string]string {
	out := map[string]string{}
	for _, e := range env {
		i := strings.IndexByte(e, '=')
		if i <= 0 || !proxyEnvName(e[:i]) {
			continue
		}
		if val := strings.TrimSpace(e[i+1:]); val != "" {
			out[e[:i]] = val
		}
	}
	return out
}

// ProxySettings 是「本机代理」的解析结果：要么来自 proxy.env 文件，要么退回
// 服务进程自己的 env。零值 = 没配代理。
type ProxySettings struct {
	Vars   map[string]string
	Source string // 文件路径，或 "进程环境"；没配置时为空
	// 是否来自 proxy.env 文件（用于面板/事件里说明来源）。
	FromFile bool
}

// loadProxySettings 解析本机代理。path 为空或 "off" = 关闭该功能。
func loadProxySettings(path string) ProxySettings {
	path = strings.TrimSpace(path)
	if path == "" || strings.EqualFold(path, "off") {
		return ProxySettings{}
	}
	if b, err := os.ReadFile(path); err == nil {
		if vars := parseProxyEnvFile(string(b)); len(vars) > 0 {
			return ProxySettings{Vars: vars, Source: path, FromFile: true}
		}
	}
	// 文件缺失/没写代理时退回进程 env：服务本身可能就是带着代理启动的。
	if vars := envProxyVars(os.Environ()); len(vars) > 0 {
		return ProxySettings{Vars: vars, Source: "进程环境"}
	}
	return ProxySettings{}
}

// Configured 表示确实有一个代理可用（只有 NO_PROXY 不算）。
func (p ProxySettings) Configured() bool {
	for name := range p.Vars {
		if !ignoresProxy(name) {
			return true
		}
	}
	return false
}

// applyTo 在 base（一般是 os.Environ()）之上叠加本机代理：同名变量（大小写不敏感）
// 先摘掉再写我们的值，保证 proxy.env 说的算，而不是被继承来的旧值盖掉。
func (p ProxySettings) applyTo(base []string) []string {
	if len(p.Vars) == 0 {
		return base
	}
	overridden := make(map[string]bool, len(p.Vars))
	for name := range p.Vars {
		overridden[strings.ToUpper(name)] = true
	}
	out := make([]string, 0, len(base)+len(p.Vars))
	for _, e := range base {
		if i := strings.IndexByte(e, '='); i > 0 && overridden[strings.ToUpper(e[:i])] {
			continue
		}
		out = append(out, e)
	}
	names := make([]string, 0, len(p.Vars))
	for name := range p.Vars {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out = append(out, name+"="+p.Vars[name])
	}
	return out
}

// Label 返回给事件/面板看的短描述，例如
// `HTTPS_PROXY=http://127.0.0.1:7897（data/proxy.env）`。http://user:pass@host
// 形式的凭证会被打码。
func (p ProxySettings) Label() string {
	if !p.Configured() {
		return ""
	}
	for _, name := range []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "https_proxy", "http_proxy", "all_proxy"} {
		if v, ok := p.Vars[name]; ok {
			return name + "=" + maskProxyCredentials(v) + "（" + p.Source + "）"
		}
	}
	return "（" + p.Source + "）"
}

// maskProxyCredentials 把代理 URL 里的 user:pass@ 打码，避免写进时间线/日志。
func maskProxyCredentials(raw string) string {
	at := strings.LastIndex(raw, "@")
	if at < 0 {
		return raw
	}
	scheme := strings.Index(raw, "://")
	if scheme < 0 || scheme+3 > at {
		return raw
	}
	return raw[:scheme+3] + "***:***@" + raw[at+1:]
}

// proxyConfigured 报告本机是否有可用的代理（/api/meta 与面板默认勾选状态）。
func proxyConfigured(cfg Config) bool {
	return loadProxySettings(cfg.ProxyEnvFile).Configured()
}
