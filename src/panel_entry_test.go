package main

import (
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
