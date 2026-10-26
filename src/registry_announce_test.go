package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRegistryOSName(t *testing.T) {
	mac, err := registryOSName("darwin")
	if err != nil || mac != "mac" {
		t.Fatalf("darwin = %q, %v", mac, err)
	}
	linux, err := registryOSName("linux")
	if err != nil || linux != "linux" {
		t.Fatalf("linux = %q, %v", linux, err)
	}
	if _, err := registryOSName("freebsd"); err == nil {
		t.Fatal("freebsd should be rejected")
	}
}

func TestRegistryArchName(t *testing.T) {
	if got := registryArchName("aarch64"); got != "arm64" {
		t.Fatalf("aarch64 = %q", got)
	}
	if got := registryArchName("x86_64"); got != "amd64" {
		t.Fatalf("x86_64 = %q", got)
	}
	if got := registryArchName("ppc64"); got != "" {
		t.Fatalf("unknown arch = %q, want empty", got)
	}
}

func TestAnnounceHost(t *testing.T) {
	if got := announceHost("0.0.0.0"); got != "" {
		t.Fatalf("wildcard = %q", got)
	}
	if got := announceHost("::"); got != "" {
		t.Fatalf("ipv6 wildcard = %q", got)
	}
	if got := announceHost("127.0.0.1"); got != "127.0.0.1" {
		t.Fatalf("loopback = %q", got)
	}
	if got := announceHost("2001:db8::1"); got != "[2001:db8::1]" {
		t.Fatalf("ipv6 = %q", got)
	}
}

func TestAnnounceDeployment(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotType string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotType = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"instance":{"id":"inst_1","host":"127.0.0.1","port":4220,"os":"mac","arch":"arm64"},"created":true}`))
	}))
	t.Cleanup(srv.Close)

	prevOS, prevArch := goos, goarch
	goos = func() string { return "darwin" }
	goarch = func() string { return "arm64" }
	t.Cleanup(func() {
		goos = prevOS
		goarch = prevArch
	})

	reg := NewServiceRegistry(Config{
		ServiceRegistryURL:     srv.URL,
		ServiceRegistryToken:   "tok",
		ServiceRegistryTimeout: 2 * time.Second,
	})
	cfg := Config{
		Host:              "127.0.0.1",
		RegistryNamespace: "default",
		RegistryService:   "agent-control-plane-deployment",
	}
	id, err := announceDeployment(context.Background(), reg, cfg, 4220)
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	if id != "inst_1" {
		t.Fatalf("id = %q", id)
	}
	if gotMethod != http.MethodPut || gotPath != "/v1/namespaces/default/services/agent-control-plane-deployment/announce" {
		t.Fatalf("request = %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if !strings.Contains(gotType, "application/json") {
		t.Fatalf("content-type = %q", gotType)
	}
	if gotBody["port"] != float64(4220) || gotBody["os"] != "mac" || gotBody["arch"] != "arm64" || gotBody["host"] != "127.0.0.1" {
		t.Fatalf("body = %#v", gotBody)
	}
}

func TestAnnounceDeploymentIdempotentOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"instance":{"id":"inst_same","host":"10.0.0.5","port":4220},"created":false}`))
	}))
	t.Cleanup(srv.Close)
	reg := registryClientFor(srv.URL)
	cfg := Config{Host: "10.0.0.5", RegistryNamespace: "default", RegistryService: "agent-control-plane-deployment"}
	id, err := announceDeployment(context.Background(), reg, cfg, 4220)
	if err != nil || id != "inst_same" {
		t.Fatalf("id=%q err=%v", id, err)
	}
}

func TestAnnounceDeploymentOmitsWildcardHost(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"instance":{"id":"inst_peer","host":"127.0.0.1","port":4220},"created":true}`))
	}))
	t.Cleanup(srv.Close)
	reg := registryClientFor(srv.URL)
	cfg := Config{Host: "0.0.0.0", RegistryNamespace: "default", RegistryService: "svc"}
	if _, err := announceDeployment(context.Background(), reg, cfg, 4220); err != nil {
		t.Fatal(err)
	}
	if _, ok := gotBody["host"]; ok {
		t.Fatalf("wildcard bind must omit host, body=%#v", gotBody)
	}
}

func TestAnnounceDeploymentFailureAborts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"先登记契约"}}`))
	}))
	t.Cleanup(srv.Close)
	reg := registryClientFor(srv.URL)
	cfg := Config{Host: "127.0.0.1", RegistryNamespace: "default", RegistryService: "missing"}
	id, err := announceDeployment(context.Background(), reg, cfg, 4220)
	if err == nil {
		t.Fatal("404 must fail startup")
	}
	if id != "" {
		t.Fatalf("id = %q, want empty", id)
	}
	if !strings.Contains(err.Error(), "先登记契约") {
		t.Fatalf("err = %v", err)
	}
}

func TestAnnounceDeploymentContinuesWhenRouteMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"未知路径 /v1/namespaces/default/services/agent-control-plane-deployment/announce"}}`))
	}))
	t.Cleanup(srv.Close)
	reg := registryClientFor(srv.URL)
	cfg := Config{Host: "0.0.0.0", RegistryNamespace: "default", RegistryService: "agent-control-plane-deployment"}
	id, err := announceDeployment(context.Background(), reg, cfg, 4220)
	if err != nil || id != "" {
		t.Fatalf("old registry must not block startup, id=%q err=%v", id, err)
	}
}

func TestAnnounceDeploymentSkipsWhenRegistryOff(t *testing.T) {
	id, err := announceDeployment(context.Background(), nil, Config{}, 4220)
	if err != nil || id != "" {
		t.Fatalf("disabled registry id=%q err=%v", id, err)
	}
}

func TestDeleteInstance(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	reg := registryClientFor(srv.URL)
	cfg := Config{RegistryNamespace: "default", RegistryService: "agent-control-plane-deployment"}
	if err := deregisterDeployment(context.Background(), reg, cfg, "inst_1"); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/v1/namespaces/default/services/agent-control-plane-deployment/instances/inst_1" {
		t.Fatalf("request = %s %s", gotMethod, gotPath)
	}
	// 404 means the address is already gone.
	missing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"gone"}}`))
	}))
	t.Cleanup(missing.Close)
	if err := deregisterDeployment(context.Background(), registryClientFor(missing.URL), cfg, "inst_1"); err != nil {
		t.Fatal(err)
	}
	if err := deregisterDeployment(context.Background(), reg, cfg, ""); err != nil {
		t.Fatal(err)
	}
}

func TestConfigRegistryIdentity(t *testing.T) {
	t.Setenv("DEPLOYMENT_HOME", t.TempDir())
	t.Setenv("REGISTRY_NS", "")
	t.Setenv("REGISTRY_SERVICE", "")
	cfg := loadConfig()
	if cfg.RegistryNamespace != "default" || cfg.RegistryService != "agent-control-plane-deployment" {
		t.Fatalf("defaults = %s/%s", cfg.RegistryNamespace, cfg.RegistryService)
	}
	t.Setenv("REGISTRY_NS", "team-a")
	t.Setenv("REGISTRY_SERVICE", "agent-control-plane-deployment")
	cfg = loadConfig()
	if cfg.RegistryNamespace != "team-a" || cfg.RegistryService != "agent-control-plane-deployment" {
		t.Fatalf("override = %s/%s", cfg.RegistryNamespace, cfg.RegistryService)
	}
}
