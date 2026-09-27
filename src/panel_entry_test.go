package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestPanelEntryExposesServiceContractFrontend verifies the server-side panel
// entry that carries the 服务契约 (service contract) frontend: root and
// /panel redirect to /panel/, and /panel/ serves the panel HTML that exposes
// the service-contract tab and table.
func TestPanelEntryExposesServiceContractFrontend(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	webDir := filepath.Join(filepath.Dir(filepath.Dir(testFile)), "web")
	srv := &apiServer{cfg: Config{WebDir: webDir}}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("GET / status = %d, want %d", rec.Code, http.StatusFound)
	}
	if got := rec.Header().Get("Location"); got != "/panel/" {
		t.Fatalf("GET / Location = %q, want /panel/", got)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/panel", nil)
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("GET /panel status = %d, want %d", rec.Code, http.StatusFound)
	}
	if got := rec.Header().Get("Location"); got != "/panel/" {
		t.Fatalf("GET /panel Location = %q, want /panel/", got)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/panel/", nil)
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /panel/ status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	for _, want := range []string{`data-tab="services"`, `id="svc-table"`, "服务契约"} {
		if !strings.Contains(body, want) {
			t.Fatalf("GET /panel/ body does not contain %q", want)
		}
	}
}

// TestPanelAndAPIAreUncacheable: 面板是就地升级的静态文件、API 是实时状态，两者都
// 不能被浏览器缓存 —— 否则升级后会出现「新 index.html + 旧 app.js」的半新半旧页面
// （新增的「部署机器」下拉就是没人填充），或者更糟：拿旧 /api/meta 渲染新面板。
func TestPanelAndAPIAreUncacheable(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	webDir := filepath.Join(filepath.Dir(filepath.Dir(testFile)), "web")
	srv := &apiServer{cfg: Config{WebDir: webDir}}

	for _, path := range []string{"/panel/", "/panel/app.js", "/panel/styles.css", "/api/meta"} {
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", path, rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
			t.Fatalf("GET %s Cache-Control = %q, want no-store", path, got)
		}
	}
}

// TestMetaExposesDeployMachines: 面板的「部署机器」下拉读的就是这里的列表，空列表
// 会让下拉空着，所以缺省也必须是单机 local。
func TestMetaExposesDeployMachines(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	webDir := filepath.Join(filepath.Dir(filepath.Dir(testFile)), "web")
	srv := &apiServer{cfg: Config{WebDir: webDir}}
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/meta", nil))
	var meta struct {
		DeployMachines       []string `json:"deployMachines"`
		DefaultDeployMachine string   `json:"defaultDeployMachine"`
		PanelVersion         string   `json:"panelVersion"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatalf("decode /api/meta: %v", err)
	}
	if len(meta.DeployMachines) == 0 {
		t.Fatal("/api/meta deployMachines must never be empty (panel renders it as the 部署机器 dropdown)")
	}
	if meta.DefaultDeployMachine == "" {
		t.Fatal("/api/meta defaultDeployMachine must not be empty")
	}
	if meta.PanelVersion == "" {
		t.Fatal("/api/meta panelVersion must be exposed so an open page can notice a redeploy")
	}
}

// TestPanelVersionTracksAssets: panelVersion 是面板静态资源的指纹 —— 面板靠它发现
// 「我这一页跑的是升级前的 JS」并自动刷新，所以「内容不变 → 指纹不变」「内容变了 →
// 指纹变」都必须成立，没有资源时则给空串（面板不比对空值）。
func TestPanelVersionTracksAssets(t *testing.T) {
	webDir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(webDir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("index.html", "<html>panel</html>")
	srv := &apiServer{cfg: Config{WebDir: webDir}}

	v1 := srv.panelVersion()
	if v1 == "" {
		t.Fatal("panelVersion must not be empty when the panel assets exist")
	}
	if again := srv.panelVersion(); again != v1 {
		t.Fatalf("panelVersion must be stable for unchanged assets: %q vs %q", v1, again)
	}
	write("app.js", "// a redeploy changed me\n")
	if v2 := srv.panelVersion(); v2 == v1 {
		t.Fatal("panelVersion must change when the panel assets change (that's what triggers the reload)")
	}
	empty := (&apiServer{cfg: Config{WebDir: filepath.Join(webDir, "missing")}}).panelVersion()
	if empty != "" {
		t.Fatalf("no assets → empty panelVersion, got %q", empty)
	}
}
