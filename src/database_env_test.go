package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestNormalizeDatabase(t *testing.T) {
	got, err := NormalizeDatabase(DatabaseConnection{})
	if err != nil || got != (DatabaseConnection{}) {
		t.Fatalf("empty = %#v %v", got, err)
	}
	got, err = NormalizeDatabase(DatabaseConnection{Env: " DATABASE_URL ", DSN: " postgres://db/app "})
	if err != nil {
		t.Fatal(err)
	}
	if got.Env != "DATABASE_URL" || got.DSN != "postgres://db/app" {
		t.Fatalf("normalized = %#v", got)
	}
	for _, in := range []DatabaseConnection{
		{Env: "DATABASE_URL"},
		{DSN: "postgres://db/app"},
		{Env: "PORT", DSN: "x"},
		{Env: "HAS-DASH", DSN: "x"},
		{Env: "DATABASE_URL", DSN: "a\nb"},
	} {
		if _, err := NormalizeDatabase(in); err == nil {
			t.Fatalf("%#v must be rejected", in)
		}
	}
}

func TestDatabaseEnvPersistsAndPutSemantics(t *testing.T) {
	store := newTestStore(t)
	svc := localConfig("event-center", "")
	svc.Port = 4438
	svc.Database = DatabaseConnection{Env: " ORG_DATABASE_URL ", DSN: " postgres://db/org "}
	saved, err := store.UpsertService(svc)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Database.Env != "ORG_DATABASE_URL" || saved.Database.DSN != "postgres://db/org" {
		t.Fatalf("upsert database = %#v", saved.Database)
	}

	api := &apiServer{store: store}
	rec := putServiceJSON(t, api, "event-center", `{
		"runtimeDir":"/tmp/event-center","healthUrl":"/health","port":4438,
		"startCmd":"true","stopCmd":"true","restartCmd":"true"
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("omit database = %d %s", rec.Code, rec.Body.String())
	}
	kept, _ := store.GetService("event-center")
	if kept == nil || kept.Database.DSN != "postgres://db/org" {
		t.Fatalf("omitting database must keep it, got %#v", kept)
	}

	rec = putServiceJSON(t, api, "event-center", `{
		"runtimeDir":"/tmp/event-center","healthUrl":"/health","port":4438,
		"startCmd":"true","stopCmd":"true","restartCmd":"true",
		"database":{"env":"","dsn":""}
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear database = %d %s", rec.Code, rec.Body.String())
	}
	cleared, _ := store.GetService("event-center")
	if cleared == nil || cleared.Database != (DatabaseConnection{}) {
		t.Fatalf("empty database must clear, got %#v", cleared)
	}

	rec = putServiceJSON(t, api, "event-center", `{
		"runtimeDir":"/tmp/event-center","healthUrl":"/health","port":4438,
		"startCmd":"true","stopCmd":"true","restartCmd":"true",
		"database":{"env":"DATABASE_URL"}
	}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "同时填写") {
		t.Fatalf("incomplete database = %d %s", rec.Code, rec.Body.String())
	}

	rec = putServiceJSON(t, api, "event-center", `{
		"runtimeDir":"/tmp/event-center","healthUrl":"/health","port":4438,
		"startCmd":"true","stopCmd":"true","restartCmd":"true",
		"runtimeEnv":{"ORG_DATABASE_URL":"postgres://from-env"},
		"database":{"env":"ORG_DATABASE_URL","dsn":"postgres://from-db"}
	}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "只保留一处") {
		t.Fatalf("conflict = %d %s", rec.Code, rec.Body.String())
	}

	rec = putServiceJSON(t, api, "event-center", `{
		"runtimeDir":"/tmp/event-center","healthUrl":"/health","port":4438,
		"startCmd":"true","stopCmd":"true","restartCmd":"true",
		"database":{"env":"ORG_DATABASE_URL","dsn":"postgres://db/org"}
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT database = %d %s", rec.Code, rec.Body.String())
	}
	var body ServiceContract
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Database.Env != "ORG_DATABASE_URL" || body.Database.DSN != "postgres://db/org" {
		t.Fatalf("response database = %#v", body.Database)
	}
}

func TestDatabaseEnvInjectedOnStartAndMaskedInTimeline(t *testing.T) {
	dsn := "postgres://app:s3cret@db.example:5432/app"
	svc := ServiceContract{
		Port:       4211,
		HealthURL:  "/health",
		RuntimeDir: "/tmp/x",
		RestartCmd: "bash scripts/restart.sh",
		RuntimeEnv: map[string]string{"LOG_LEVEL": "debug"},
		Database:   DatabaseConnection{Env: "DATABASE_URL", DSN: dsn},
	}
	joined := "\n" + strings.Join(serviceCmdEnv(svc, "/tmp/x", map[string]string{
		"APP_VERSION": "abc", "DEPLOY_MACHINE": "local", "RUNTIME_DIR": "/tmp/x",
	}), "\n") + "\n"
	if !strings.Contains(joined, "\nDATABASE_URL="+dsn+"\n") {
		t.Fatalf("start env missing database DSN:\n%s", joined)
	}
	if !strings.Contains(joined, "\nLOG_LEVEL=debug\n") || !strings.Contains(joined, "\nPORT=4211\n") {
		t.Fatalf("database injection must keep the other env:\n%s", joined)
	}

	extra := map[string]string{"APP_VERSION": "abc", "DEPLOY_MACHINE": "local", "RUNTIME_DIR": "/tmp/x"}
	actual := remoteRestartCmd(svc.RestartCmd, svc, extra)
	if !strings.Contains(actual, "DATABASE_URL="+shellQuote(dsn)) {
		t.Fatalf("remote command must inject the DSN:\n%s", actual)
	}
	shown := shownServiceCmd(svc.RestartCmd, "", svc, "/tmp/x", "abc", "local")
	if strings.Contains(shown, "s3cret") || strings.Contains(shown, dsn) {
		t.Fatalf("timeline must not contain the DSN:\n%s", shown)
	}
	if !strings.Contains(shown, "DATABASE_URL='***'") {
		t.Fatalf("timeline must show the database env name:\n%s", shown)
	}
	if suffix := runtimeEnvEventSuffix(svc); suffix != "，运行时环境变量 LOG_LEVEL，数据库连接 DATABASE_URL" {
		t.Fatalf("event suffix = %q", suffix)
	}

	// 同名脏数据：数据库连接盖过 runtimeEnv，平台键仍然最后生效。
	override := ServiceContract{
		Port:       4211,
		RuntimeEnv: map[string]string{"DATABASE_URL": "stale", "PORT": "9"},
		Database:   DatabaseConnection{Env: "DATABASE_URL", DSN: dsn},
	}
	joined = "\n" + strings.Join(serviceCmdEnv(override, "/tmp/x", nil), "\n") + "\n"
	if strings.Contains(joined, "DATABASE_URL=stale") || !strings.Contains(joined, "\nDATABASE_URL="+dsn+"\n") {
		t.Fatalf("database connection must win over runtimeEnv:\n%s", joined)
	}
	if strings.Contains(joined, "\nPORT=9\n") {
		t.Fatalf("platform PORT must still win:\n%s", joined)
	}
}
