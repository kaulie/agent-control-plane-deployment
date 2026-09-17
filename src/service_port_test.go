package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func newPortTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "port.sqlite"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// 部署契约「服务端口」：单独一项，写库/读库都保留；越界直接 400。
func TestPutServicePort(t *testing.T) {
	store := newPortTestStore(t)
	if _, err := store.UpsertService(localConfig("web-cursor", "")); err != nil {
		t.Fatalf("UpsertService: %v", err)
	}
	// 已有本地配置 → 不需要注册中心就能改（这样测试不依赖 fake registry）。
	api := &apiServer{store: store}
	base := `{"runtimeDir":"/tmp/web-cursor","healthUrl":"http://127.0.0.1:4211/health",` +
		`"startCmd":"true","stopCmd":"true","restartCmd":"true"`

	setPort := func(p string) (int, string) {
		t.Helper()
		rec := putServiceJSON(t, api, "web-cursor", base+`,"port":`+p+`}`)
		return rec.Code, rec.Body.String()
	}

	code, body := setPort("4212")
	if code != http.StatusOK {
		t.Fatalf("PUT port=4212 = %d, body=%s", code, body)
	}
	var got ServiceContract
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Port != 4212 {
		t.Fatalf("response port = %d, want 4212", got.Port)
	}
	stored, err := store.GetService("web-cursor")
	if err != nil || stored == nil {
		t.Fatalf("GetService: %v (%v)", err, stored)
	}
	if stored.Port != 4212 {
		t.Fatalf("stored port = %d, want 4212", stored.Port)
	}

	// 不带 port 的更新不会把已有端口清掉。
	if code, body = setPort("4212"); code != http.StatusOK {
		t.Fatalf("PUT again = %d, body=%s", code, body)
	}
	rec := putServiceJSON(t, api, "web-cursor", base+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT without port = %d, body=%s", rec.Code, rec.Body.String())
	}
	if stored, _ = store.GetService("web-cursor"); stored.Port != 4212 {
		t.Fatalf("port should survive a PUT that omits it, got %d", stored.Port)
	}

	// 0 / 缺失 = 未指定 → 400（服务端口是必填项）。
	if code, body = setPort("0"); code != http.StatusBadRequest {
		t.Fatalf("PUT port=0 = %d, want 400 (body=%s)", code, body)
	}
	// 库里没有有效端口时，不带 port 的 PUT 也必须 400。
	store2 := newPortTestStore(t)
	noPort := localConfig("no-port", "")
	noPort.Port = 0 // 显式清掉，构造"契约里没有端口"的老数据
	if _, err := store2.UpsertService(noPort); err != nil {
		t.Fatalf("UpsertService: %v", err)
	}
	api2 := &apiServer{store: store2}
	rec2 := putServiceJSON(t, api2, "no-port", base+`}`)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("PUT without port on a contract that has none = %d, want 400", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), "SERVICE_PORT") {
		t.Fatalf("error should mention SERVICE_PORT: %s", rec2.Body.String())
	}

	// 越界 → 400（不写库）。
	for _, bad := range []string{"70000", "-1"} {
		code, body = setPort(bad)
		if code != http.StatusBadRequest {
			t.Fatalf("PUT port=%s = %d, want 400 (body=%s)", bad, code, body)
		}
		if !strings.Contains(body, "port") {
			t.Fatalf("error for port=%s should mention port: %s", bad, body)
		}
	}
}

func TestStoreServicePortRoundTrip(t *testing.T) {
	store := newPortTestStore(t)
	svc := localConfig("svc-port", "")
	svc.Port = 4213
	if _, err := store.UpsertService(svc); err != nil {
		t.Fatalf("UpsertService: %v", err)
	}
	got, err := store.GetService("svc-port")
	if err != nil || got == nil {
		t.Fatalf("GetService: %v (%v)", err, got)
	}
	if got.Port != 4213 {
		t.Fatalf("port = %d, want 4213", got.Port)
	}
	list, err := store.ListServices()
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	found := false
	for _, s := range list {
		if s.ServiceID == "svc-port" {
			found = true
			if s.Port != 4213 {
				t.Fatalf("list port = %d, want 4213", s.Port)
			}
		}
	}
	if !found {
		t.Fatal("service missing from ListServices")
	}

	// 非法端口落库时收敛成 0（不会写进奇怪的值）。
	svc.Port = 99999
	if _, err := store.UpsertService(svc); err != nil {
		t.Fatalf("UpsertService(invalid): %v", err)
	}
	if got, _ = store.GetService("svc-port"); got.Port != 0 {
		t.Fatalf("invalid port should be normalized to 0, got %d", got.Port)
	}
}

// 给 start/stop/restart 脚本的端口：注入 SERVICE_PORT（正式字段名）+ PORT（兼容），
// 取值 = 契约里显式配置的 port；没配置（老契约）才退回 healthUrl 推导。
func TestServicePortPrecedence(t *testing.T) {
	cases := []struct {
		name string
		svc  ServiceContract
		want string
	}{
		{"explicit port wins", ServiceContract{Port: 4212, HealthURL: "http://127.0.0.1:4211/health"}, "4212"},
		{"unset falls back to healthUrl", ServiceContract{HealthURL: "http://127.0.0.1:4211/health"}, "4211"},
		{"invalid port falls back", ServiceContract{Port: 70000, HealthURL: "http://127.0.0.1:4211/health"}, "4211"},
	}
	for _, c := range cases {
		if got := servicePort(c.svc); got != c.want {
			t.Errorf("%s: servicePort = %q, want %q", c.name, got, c.want)
		}
	}

	env := serviceCmdEnv(ServiceContract{Port: 4212, HealthURL: "http://127.0.0.1:4211/health", RuntimeDir: "/tmp/x"}, nil)
	joined := "\n" + strings.Join(env, "\n") + "\n"
	for _, want := range []string{"\nSERVICE_PORT=4212\n", "\nPORT=4212\n", "\nRUNTIME_DIR=/tmp/x\n"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("restart env should contain %q:\n%s", want, joined)
		}
	}

	// 老契约（未指定 port）也必须有 SERVICE_PORT（按 healthUrl 推导），不能漏注入。
	legacy := "\n" + strings.Join(serviceCmdEnv(ServiceContract{HealthURL: "http://127.0.0.1:4211/health"}, nil), "\n") + "\n"
	if !strings.Contains(legacy, "\nSERVICE_PORT=4211\n") {
		t.Fatalf("legacy contract must still get SERVICE_PORT:\n%s", legacy)
	}
}
