package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLooksLikeNotRunning(t *testing.T) {
	yes := []string{
		"[restart] stop\n[stop] 没有运行中的 Brain\n[restart]",
		"[stop] 没有运行中的能力集市服务",
		"[stop] not running",
		"Job is not running",
		"service isn't running yet",
		"unit not currently running",
	}
	for _, s := range yes {
		if !looksLikeNotRunning(s) {
			t.Fatalf("expected not-running: %q", s)
		}
	}
	no := []string{
		"[start] ok pid=12",
		"address already in use",
		"permission denied",
		"",
	}
	for _, s := range no {
		if looksLikeNotRunning(s) {
			t.Fatalf("did not expect not-running: %q", s)
		}
	}
}

func TestSeedMissingBackendEnvCopiesExampleOnce(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "server"), 0o755); err != nil {
		t.Fatal(err)
	}
	example := []byte("ARK_API_KEY=\n")
	if err := os.WriteFile(filepath.Join(dir, "server", ".env.example"), example, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := seedMissingBackendEnv(dir)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got != "server/.env.example" {
		t.Fatalf("seeded from %q, want server/.env.example", got)
	}
	dest := filepath.Join(dir, "backend", ".env")
	body, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(body) != string(example) {
		t.Fatalf("dest body = %q", body)
	}

	// 已有文件不得覆盖。
	if err := os.WriteFile(dest, []byte("ARK_API_KEY=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = seedMissingBackendEnv(dir)
	if err != nil {
		t.Fatalf("second seed: %v", err)
	}
	if got != "" {
		t.Fatalf("must not re-seed, got %q", got)
	}
	body, _ = os.ReadFile(dest)
	if string(body) != "ARK_API_KEY=secret\n" {
		t.Fatalf("existing .env was overwritten: %q", body)
	}
}

func TestSeedMissingBackendEnvNoTemplateIsNoop(t *testing.T) {
	got, err := seedMissingBackendEnv(t.TempDir())
	if err != nil || got != "" {
		t.Fatalf("got %q err=%v, want empty", got, err)
	}
}

func TestSeedMissingBackendEnvRemoteScriptDoesNotOverwrite(t *testing.T) {
	script := seedMissingBackendEnvRemoteScript("/home/ubuntu/runtime/home-agent-brain")
	if !strings.Contains(script, "backend/.env already present") {
		t.Fatalf("script must skip when .env exists:\n%s", script)
	}
	if !strings.Contains(script, "server/.env.example") {
		t.Fatalf("script must try the Brain template:\n%s", script)
	}
	if strings.Contains(script, "exit 1") {
		t.Fatal("seeding must not fail the deploy when no template exists")
	}
}
