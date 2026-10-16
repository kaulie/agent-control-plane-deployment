package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestNormalizeRuntimeEnv(t *testing.T) {
	got, err := NormalizeRuntimeEnv(nil)
	if err != nil || got != nil {
		t.Fatalf("nil → (%v, %v), want nil, nil", got, err)
	}
	got, err = NormalizeRuntimeEnv(map[string]string{" LOG_LEVEL ": "debug", "EMPTY": ""})
	if err != nil {
		t.Fatal(err)
	}
	if got["LOG_LEVEL"] != "debug" || got["EMPTY"] != "" {
		t.Fatalf("normalized = %#v", got)
	}

	for _, in := range []map[string]string{
		{"": "x"},
		{"1PORT": "x"},
		{"HAS-DASH": "x"},
		{"SERVICE_PORT": "4211"},
		{"PORT": "9"},
		{"APP_VERSION": "abc"},
		{"DEPLOY_MACHINE": "local"},
		{"RUNTIME_DIR": "/tmp"},
		{"BROKEN": "a\nb"},
	} {
		if _, err := NormalizeRuntimeEnv(in); err == nil {
			t.Fatalf("%#v must be rejected", in)
		}
	}
}

func TestRuntimeEnvPersistsAndPutSemantics(t *testing.T) {
	store := newTestStore(t)

	svc := localConfig("event-center", "")
	svc.Port = 4438
	svc.RuntimeEnv = map[string]string{" LOG_LEVEL ": "info"}
	saved, err := store.UpsertService(svc)
	if err != nil {
		t.Fatal(err)
	}
	if saved.RuntimeEnv["LOG_LEVEL"] != "info" || len(saved.RuntimeEnv) != 1 {
		t.Fatalf("upsert normalized runtimeEnv = %#v", saved.RuntimeEnv)
	}
	got, err := store.GetService("event-center")
	if err != nil || got == nil || got.RuntimeEnv["LOG_LEVEL"] != "info" {
		t.Fatalf("GetService runtimeEnv = %#v err=%v", got, err)
	}

	api := &apiServer{store: store}
	// 不传 runtimeEnv：沿用已有配置。
	rec := putServiceJSON(t, api, "event-center", `{
		"runtimeDir":"/tmp/event-center","healthUrl":"/health","port":4438,
		"startCmd":"true","stopCmd":"true","restartCmd":"true"
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("omit runtimeEnv = %d %s", rec.Code, rec.Body.String())
	}
	kept, _ := store.GetService("event-center")
	if kept == nil || kept.RuntimeEnv["LOG_LEVEL"] != "info" {
		t.Fatalf("omitting runtimeEnv must keep it, got %#v", kept)
	}

	// 显式 {}：清空。
	rec = putServiceJSON(t, api, "event-center", `{
		"runtimeDir":"/tmp/event-center","healthUrl":"/health","port":4438,
		"startCmd":"true","stopCmd":"true","restartCmd":"true",
		"runtimeEnv":{}
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear runtimeEnv = %d %s", rec.Code, rec.Body.String())
	}
	cleared, _ := store.GetService("event-center")
	if cleared == nil || len(cleared.RuntimeEnv) != 0 {
		t.Fatalf("empty runtimeEnv must clear, got %#v", cleared)
	}

	// 非法名字：400，且不改库。
	rec = putServiceJSON(t, api, "event-center", `{
		"runtimeDir":"/tmp/event-center","healthUrl":"/health","port":4438,
		"startCmd":"true","stopCmd":"true","restartCmd":"true",
		"runtimeEnv":{"SERVICE_PORT":"1"}
	}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("reserved key = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "SERVICE_PORT") {
		t.Fatalf("error should name the key: %s", rec.Body.String())
	}
	after, _ := store.GetService("event-center")
	if after == nil || len(after.RuntimeEnv) != 0 {
		t.Fatalf("rejected PUT must not write runtimeEnv, got %#v", after)
	}

	rec = putServiceJSON(t, api, "event-center", `{
		"runtimeDir":"/tmp/event-center","healthUrl":"/health","port":4438,
		"startCmd":"true","stopCmd":"true","restartCmd":"true",
		"runtimeEnv":{"AUTONOMY_HTTP_HOST":"0.0.0.0","LOG_LEVEL":"debug"}
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT runtimeEnv = %d %s", rec.Code, rec.Body.String())
	}
	var body ServiceContract
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.RuntimeEnv["AUTONOMY_HTTP_HOST"] != "0.0.0.0" || body.RuntimeEnv["LOG_LEVEL"] != "debug" {
		t.Fatalf("response runtimeEnv = %#v", body.RuntimeEnv)
	}
}

func TestRuntimeEnvInjectedOnStartAndMaskedInTimeline(t *testing.T) {
	svc := ServiceContract{
		Port:       4211,
		HealthURL:  "/health",
		RuntimeDir: "/tmp/x",
		RestartCmd: "bash scripts/restart.sh",
		RuntimeEnv: map[string]string{
			"LOG_LEVEL": "debug",
			"API_TOKEN": "s3cret",
			"PORT":      "9", // 脏数据：平台键必须被盖掉，不能挡住启动
		},
	}
	joined := "\n" + strings.Join(serviceCmdEnv(svc, "/tmp/x", map[string]string{
		"APP_VERSION": "abc", "DEPLOY_MACHINE": "local", "RUNTIME_DIR": "/tmp/x",
	}), "\n") + "\n"
	for _, want := range []string{"\nLOG_LEVEL=debug\n", "\nAPI_TOKEN=s3cret\n", "\nSERVICE_PORT=4211\n", "\nPORT=4211\n", "\nAPP_VERSION=abc\n"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("start env missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "\nPORT=9\n") {
		t.Fatalf("platform PORT must win over a stored runtimeEnv PORT:\n%s", joined)
	}

	extra := map[string]string{"APP_VERSION": "abc", "DEPLOY_MACHINE": "local", "RUNTIME_DIR": "/tmp/x"}
	actual := remoteRestartCmd(svc.RestartCmd, svc, extra)
	if !strings.Contains(actual, "LOG_LEVEL='debug'") || !strings.Contains(actual, "API_TOKEN='s3cret'") {
		t.Fatalf("remote command must inject values:\n%s", actual)
	}
	if !strings.Contains(actual, "SERVICE_PORT='4211'") || !strings.Contains(actual, "cd '/tmp/x'") {
		t.Fatalf("remote command must still cd and export platform env:\n%s", actual)
	}
	shown := shownServiceCmd(svc.RestartCmd, "", svc, "/tmp/x", "abc", "local")
	if strings.Contains(shown, "s3cret") || strings.Contains(shown, "debug") {
		t.Fatalf("timeline command must not contain runtime env values:\n%s", shown)
	}
	if !strings.Contains(shown, "LOG_LEVEL='***'") || !strings.Contains(shown, "API_TOKEN='***'") {
		t.Fatalf("timeline command must show the key names:\n%s", shown)
	}
	if !strings.Contains(shown, "SERVICE_PORT='4211'") {
		t.Fatalf("platform values stay visible:\n%s", shown)
	}
	if suffix := runtimeEnvEventSuffix(svc); suffix != "，运行时环境变量 API_TOKEN、LOG_LEVEL" {
		t.Fatalf("event suffix = %q", suffix)
	}

	// 值里的单引号按 POSIX 转义，不能把 shell 弄断。
	quoted := ServiceContract{Port: 4211, RuntimeEnv: map[string]string{"MSG": "it's"}}
	cmd := remoteRestartCmd("true", quoted, extra)
	if !strings.Contains(cmd, `MSG='it'\''s'`) {
		t.Fatalf("shell quote = %s", cmd)
	}
}
