package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本机代理选项（发起流水线时勾选）的解析与注入。

const sampleProxyEnv = `# Routes github.com / api.github.com access through the local proxy
export HTTPS_PROXY=http://127.0.0.1:7897
HTTP_PROXY="http://127.0.0.1:7897"
ALL_PROXY=http://127.0.0.1:7897
NO_PROXY=localhost,127.0.0.1,::1
GOPROXY=https://goproxy.cn,direct
EMPTY_PROXY=
`

func writeProxyEnvFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "proxy.env")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write proxy.env: %v", err)
	}
	return p
}

// 清掉进程里可能存在的代理变量，让用例不受开发机环境干扰。
func clearProxyEnv(t *testing.T) {
	t.Helper()
	for _, n := range []string{
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
		"http_proxy", "https_proxy", "all_proxy", "no_proxy",
	} {
		t.Setenv(n, "")
	}
}

func getJSON(t *testing.T, srv *apiServer, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestParseProxyEnvFile(t *testing.T) {
	got := parseProxyEnvFile(sampleProxyEnv)
	want := map[string]string{
		"HTTPS_PROXY": "http://127.0.0.1:7897",
		"HTTP_PROXY":  "http://127.0.0.1:7897",
		"ALL_PROXY":   "http://127.0.0.1:7897",
		"NO_PROXY":    "localhost,127.0.0.1,::1",
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %v, want %v (only proxy vars: no GOPROXY/EMPTY_PROXY)", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestLoadProxySettingsFileThenProcessEnv(t *testing.T) {
	clearProxyEnv(t)
	path := writeProxyEnvFile(t, sampleProxyEnv)

	fromFile := loadProxySettings(path)
	if !fromFile.Configured() || !fromFile.FromFile || fromFile.Source != path {
		t.Fatalf("file settings = %+v, want configured from %s", fromFile, path)
	}
	if label := fromFile.Label(); !strings.Contains(label, "http://127.0.0.1:7897") {
		t.Fatalf("label = %q, want the proxy url", label)
	}

	// 文件缺失 → 退回进程 env（服务自己带着代理跑的情况）。
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9999")
	fallback := loadProxySettings(filepath.Join(t.TempDir(), "missing.env"))
	if !fallback.Configured() || fallback.FromFile || fallback.Source != "进程环境" {
		t.Fatalf("fallback = %+v, want process env", fallback)
	}

	// PROXY_ENV_FILE=off 关掉整个选项（即使进程里有代理）。
	if off := loadProxySettings("off"); off.Configured() {
		t.Fatalf("off must disable the option, got %+v", off)
	}

	// 只有 NO_PROXY 不算「配了代理」。
	onlyNoProxy := loadProxySettings(writeProxyEnvFile(t, "NO_PROXY=localhost\n"))
	if onlyNoProxy.Configured() {
		t.Fatalf("NO_PROXY alone must not count as configured: %+v", onlyNoProxy)
	}
}

func TestProxySettingsApplyToOverridesInheritedValues(t *testing.T) {
	clearProxyEnv(t)
	base := []string{"PATH=/bin", "http_proxy=http://old:1", "HTTPS_PROXY=http://old:2", "FOO=bar"}
	got := loadProxySettings(writeProxyEnvFile(t, sampleProxyEnv)).applyTo(base)

	joined := strings.Join(got, " ")
	if strings.Contains(joined, "old") {
		t.Fatalf("inherited proxy values must be replaced, got %v", got)
	}
	if !strings.Contains(joined, "PATH=/bin") || !strings.Contains(joined, "FOO=bar") {
		t.Fatalf("unrelated vars must survive, got %v", got)
	}
	proxyVars := 0
	for _, e := range got {
		if strings.HasPrefix(strings.ToUpper(e), "HTTP_PROXY=") ||
			strings.HasPrefix(strings.ToUpper(e), "HTTPS_PROXY=") {
			proxyVars++
		}
	}
	if proxyVars != 2 { // HTTP_PROXY + HTTPS_PROXY from the file
		t.Fatalf("want exactly the file's proxy vars, got %v", got)
	}
}

func TestProxySettingsOnlyApplyWhenOptedIn(t *testing.T) {
	clearProxyEnv(t)
	path := writeProxyEnvFile(t, sampleProxyEnv)

	// 没勾选 → 不解析、不注入（和以前完全一样）。
	direct := PackageOptions{ProxyEnvFile: path}.useProxySettings().applyTo(os.Environ())
	for _, e := range direct {
		name, val, _ := strings.Cut(e, "=")
		if strings.EqualFold(name, "HTTPS_PROXY") && val != "" {
			t.Fatalf("without the option nothing may be injected, got %v", direct)
		}
	}

	// 勾选 + 本机有配置 → 注入。
	proxied := PackageOptions{UseProxy: true, ProxyEnvFile: path}.useProxySettings().applyTo(os.Environ())
	found := ""
	for _, e := range proxied {
		name, val, _ := strings.Cut(e, "=")
		if name == "HTTPS_PROXY" && val != "" {
			found = e
		}
	}
	if found != "HTTPS_PROXY=http://127.0.0.1:7897" {
		t.Fatalf("opt-in must inject the machine proxy, got %v", proxied)
	}

	// 勾选但本机没配置 → 仍然直连，并被 packageFromGit 记成 warn。
	missing := PackageOptions{UseProxy: true, ProxyEnvFile: filepath.Join(t.TempDir(), "none.env")}.useProxySettings()
	if missing.Configured() {
		t.Fatalf("missing config must not be reported as configured: %+v", missing)
	}
}

func TestMaskProxyCredentials(t *testing.T) {
	if got := maskProxyCredentials("http://user:secret@127.0.0.1:7897"); got != "http://***:***@127.0.0.1:7897" {
		t.Fatalf("maskProxyCredentials = %q", got)
	}
	if got := maskProxyCredentials("http://127.0.0.1:7897"); got != "http://127.0.0.1:7897" {
		t.Fatalf("credential-free url must stay readable, got %q", got)
	}
}

func TestDeployNotifyPersistsUseProxy(t *testing.T) {
	srv, store := newTestIdentityServer(t, true)
	rec := postJSON(t, srv, "/api/deploy-notify", `{"serviceId":"web-cursor","useProxy":true}`,
		map[string]string{"identity_role": "user", "identity_id": "user_001"})
	if rec.Code != 202 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		RequestID string `json:"requestId"`
		UseProxy  bool   `json:"useProxy"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.UseProxy {
		t.Fatalf("response must echo useProxy=true, got %s", rec.Body.String())
	}
	job, err := store.GetPipeline(resp.RequestID)
	if err != nil || job == nil {
		t.Fatalf("GetPipeline(%s): job=%v err=%v", resp.RequestID, job, err)
	}
	if !job.UseProxy {
		t.Fatal("useProxy must be persisted on the pipeline (packaging happens later, in the worker)")
	}
	events, err := store.ListPipelineEvents(resp.RequestID)
	if err != nil {
		t.Fatalf("ListPipelineEvents: %v", err)
	}
	joined := ""
	for _, e := range events {
		joined += e.Message + "\n"
	}
	if !strings.Contains(joined, "本机代理") {
		t.Fatalf("the timeline should say the pack goes through the local proxy, got:\n%s", joined)
	}
}

// 选项存在流水线上，打包是稍后由 worker 干的：worker 必须把它带进 PackageOptions，
// 否则「勾了也没用」。
func TestPipelineWorkerCarriesUseProxyIntoPackaging(t *testing.T) {
	store := newDeployWorkerTestStore(t)
	cfg := Config{ReleaseMaxSec: 300, ProxyEnvFile: "/tmp/dep/data/proxy.env"}
	worker := NewPipelineWorker(store, cfg, nil, nil, nil)

	on := worker.packageOptions(&PipelineJob{RequestID: "pipeline-1", UseProxy: true}, nil)
	if !on.UseProxy || on.ProxyEnvFile != cfg.ProxyEnvFile || on.MaxSec != 300 {
		t.Fatalf("opt-in must reach packaging, got %+v", on)
	}
	off := worker.packageOptions(&PipelineJob{RequestID: "pipeline-2"}, nil)
	if off.UseProxy {
		t.Fatalf("a pipeline that did not opt in must stay direct, got %+v", off)
	}
}

func TestMetaReportsProxyConfig(t *testing.T) {
	srv, _ := newTestIdentityServer(t, true)
	clearProxyEnv(t)
	srv.cfg.ProxyEnvFile = writeProxyEnvFile(t, sampleProxyEnv)

	var meta map[string]any
	if err := json.Unmarshal(getJSON(t, srv, "/api/meta").Body.Bytes(), &meta); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	if meta["proxyConfigured"] != true {
		t.Fatalf("proxyConfigured = %v, want true", meta["proxyConfigured"])
	}
	if meta["proxyEnvFile"] != srv.cfg.ProxyEnvFile {
		t.Fatalf("proxyEnvFile = %v, want %s", meta["proxyEnvFile"], srv.cfg.ProxyEnvFile)
	}

	// 本机没有代理配置时，面板应把勾选框留空。
	srv.cfg.ProxyEnvFile = filepath.Join(t.TempDir(), "missing.env")
	meta = nil
	if err := json.Unmarshal(getJSON(t, srv, "/api/meta").Body.Bytes(), &meta); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	if meta["proxyConfigured"] != false {
		t.Fatalf("proxyConfigured = %v, want false", meta["proxyConfigured"])
	}
}
