package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	srv := &apiServer{cfg: Config{}}
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/meta", nil))
	var meta struct {
		DeployMachines       []string `json:"deployMachines"`
		DefaultDeployMachine string   `json:"defaultDeployMachine"`
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
}
