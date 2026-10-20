package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kaulie/agent-control-plane-deployment/eventlevel"
)

func TestParseGoVer(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"1.25.0", "1.25.0", true},
		{"go1.24.5", "1.24.5", true},
		{"go1.25", "1.25.0", true},
		{"go version go1.24.5 darwin/amd64", "1.24.5", true},
		{"go1.25.0-rc.1", "1.25.0", true},
		{"nope", "", false},
		{"1", "", false},
	}
	for _, tc := range cases {
		got, ok := parseGoVer(tc.in)
		if ok != tc.ok || (ok && got.String() != tc.want) {
			t.Fatalf("parseGoVer(%q) = (%s, %v), want (%s, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
	older, _ := parseGoVer("1.24.5")
	newer, _ := parseGoVer("1.25.0")
	if older.cmp(newer) >= 0 {
		t.Fatalf("1.24.5 should be older than 1.25.0")
	}
}

func TestRequiredGoVer(t *testing.T) {
	mod := `
module example.com/m

go 1.24.0

toolchain go1.25.0

// go 1.99.0
`
	got, ok := requiredGoVer(mod)
	if !ok || got.String() != "1.25.0" {
		t.Fatalf("requiredGoVer = (%s, %v), want 1.25.0", got, ok)
	}
	if _, ok := requiredGoVer("module example.com/m\n"); ok {
		t.Fatal("a module without a go directive has no required version")
	}
}

func TestEnvWithout(t *testing.T) {
	got := envWithout([]string{"A=1", "GOTOOLCHAIN=local", "B=2", "GOROOT=/sdk"}, "GOTOOLCHAIN", "GOROOT")
	if strings.Join(got, ",") != "A=1,B=2" {
		t.Fatalf("envWithout = %v", got)
	}
}

func TestEnsureGoToolchainNoModule(t *testing.T) {
	pick, err := ensureGoToolchain(t.TempDir())
	if err != nil || pick.binDir != "" {
		t.Fatalf("no go.mod: pick=%+v err=%v", pick, err)
	}
}

func TestEnsureGoToolchainSkipsWhenLocalIsNewEnough(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module m\n\ngo 1.21.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pick, err := ensureGoToolchain(dir)
	if err != nil {
		t.Fatal(err)
	}
	if pick.binDir != "" {
		t.Fatalf("local go should satisfy 1.21.0, got %+v", pick)
	}
}

func TestEnsureGoToolchainSelectsNewer(t *testing.T) {
	dir := t.TempDir()
	const need = "1.25.0"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module m\n\ngo "+need+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pick, err := ensureGoToolchain(dir)
	if err != nil {
		t.Fatal(err)
	}
	cur, err := currentGoVersion()
	if err != nil {
		t.Fatal(err)
	}
	req, _ := parseGoVer(need)
	if cur.cmp(req) >= 0 {
		if pick.binDir != "" {
			t.Fatalf("local go %s already satisfies %s, got %+v", cur, need, pick)
		}
		return
	}
	if pick.binDir == "" || pick.root == "" {
		t.Fatalf("expected a toolchain >= %s, got %+v", need, pick)
	}
	if !strings.Contains(pick.note, "GOTOOLCHAIN") {
		t.Fatalf("note should explain GOTOOLCHAIN=local, got %q", pick.note)
	}
	cmd := exec.Command(filepath.Join(pick.binDir, "go"), "env", "GOVERSION")
	cmd.Env = append(envWithout(os.Environ(), "GOTOOLCHAIN", "GOROOT", "GOMODCACHE"),
		"GOTOOLCHAIN=local", "GOROOT="+pick.root)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("selected go: %v", err)
	}
	got, ok := parseGoVer(strings.TrimSpace(string(out)))
	if !ok || got.cmp(req) < 0 {
		t.Fatalf("selected go version %q, want >= %s", strings.TrimSpace(string(out)), need)
	}
}

// 服务 build.sh 把 GOTOOLCHAIN 设成 local 时，仍然要跑到我们放上 PATH 的 go。
func TestPackageFromGitUsesSelectedGoWhenBuildPinsLocalToolchain(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(bin, "go"), "#!/bin/sh\nif [ \"$1\" = \"version\" ]; then echo \"go version go1.25.0 darwin/amd64\"; exit 0; fi\necho \"unexpected $*\" >&2\nexit 1\n", 0o755)

	orig := lookupGoToolchain
	lookupGoToolchain = func(string) (goToolchainPick, error) {
		return goToolchainPick{binDir: bin, root: root, note: "使用 go1.25.0"}, nil
	}
	t.Cleanup(func() { lookupGoToolchain = orig })

	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	buildSh := "#!/usr/bin/env bash\nset -euo pipefail\nif [ -z \"${GOTOOLCHAIN:-}\" ]; then export GOTOOLCHAIN=local; fi\nmkdir -p outputs\ngo version > outputs/GOVER\nprintf '%s\\n' \"$GOROOT\" > outputs/GOROOT\ncommand -v go > outputs/WHICH\n"
	mustWrite(t, filepath.Join(repo, "build.sh"), buildSh, 0o755)
	mustWrite(t, filepath.Join(repo, "README.md"), "fixture\n", 0o644)
	runGit(t, repo, "init", "-q", "-b", "main")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "-qm", "init")

	pkgs := filepath.Join(root, "packages")
	var events []string
	res, err := packageFromGit("svc-go", "file://"+repo, "main", PackageOptions{
		MaxSec:  120,
		Storage: &localStorage{base: pkgs},
		Events: func(_ eventlevel.Level, msg string) {
			events = append(events, msg)
		},
	})
	if err != nil {
		t.Fatalf("packageFromGit: %v", err)
	}
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(pkgs, "svc-go", res.Tag, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return strings.TrimSpace(string(b))
	}
	if got := read("GOVER"); !strings.Contains(got, "go1.25.0") {
		t.Fatalf("build saw %q, want the selected toolchain", got)
	}
	if got := read("WHICH"); got != filepath.Join(bin, "go") {
		t.Fatalf("which go = %q, want %s", got, filepath.Join(bin, "go"))
	}
	if got := read("GOROOT"); got != root {
		t.Fatalf("GOROOT = %q, want %s", got, root)
	}
	if !strings.Contains(strings.Join(events, "\n"), "使用 go1.25.0") {
		t.Fatalf("timeline missing toolchain note: %v", events)
	}
}
