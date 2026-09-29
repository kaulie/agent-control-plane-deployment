package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParsePIDLine(t *testing.T) {
	cases := map[string]string{
		"12345\n":     "12345",
		"  99  ":      "99",
		"12\n[start]": "12",
		"":            "",
		"abc":         "",
		"12pid":       "",
	}
	for in, want := range cases {
		if got := parsePIDLine(in); got != want {
			t.Fatalf("parsePIDLine(%q)=%q want %q", in, got, want)
		}
	}
}

func TestRuntimePidPath(t *testing.T) {
	got := runtimePidPath("/home/ubuntu/runtime/brain")
	if !strings.HasSuffix(got, filepath.FromSlash("backend/runtime.pid")) {
		t.Fatalf("path = %q", got)
	}
}

func TestPlatformStopScriptUsesPidfileAndPort(t *testing.T) {
	script := platformStopScript("/rt/svc", "9527")
	for _, want := range []string{
		"backend/runtime.pid",
		"/rt/svc",
		"9527",
		"not running",
		"lsof",
		"stale pidfile",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script must contain %q:\n%s", want, script)
		}
	}
}

func TestReadRuntimePIDLocal(t *testing.T) {
	dir := t.TempDir()
	if got := readRuntimePIDLocal(dir); got != "" {
		t.Fatalf("missing file should be empty, got %q", got)
	}
	if err := os.MkdirAll(filepath.Join(dir, "backend"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimePidPath(dir), []byte("4242\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readRuntimePIDLocal(dir); got != "4242" {
		t.Fatalf("got %q want 4242", got)
	}
}

func TestReadRuntimePIDScript(t *testing.T) {
	s := readRuntimePIDScript("/rt/svc")
	if !strings.Contains(s, "backend/runtime.pid") {
		t.Fatalf("read script must name the pidfile:\n%s", s)
	}
}

func TestRunPlatformStopLocalNotRunning(t *testing.T) {
	dir := t.TempDir()
	code, out := runPlatformStop(nil, MachineTarget{Kind: "local"}, dir, "1", 5)
	if code != 0 {
		t.Fatalf("not-running stop must exit 0, got %d %q", code, out)
	}
	if !strings.Contains(out, "not running") {
		t.Fatalf("must report not running, got %q", out)
	}
}
