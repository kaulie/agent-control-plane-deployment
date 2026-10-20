package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// goVer is a Go release (1.24.5). A missing patch component is 0.
type goVer struct {
	major, minor, patch int
}

func (v goVer) String() string {
	return fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch)
}

func (v goVer) cmp(o goVer) int {
	if v.major != o.major {
		return v.major - o.major
	}
	if v.minor != o.minor {
		return v.minor - o.minor
	}
	return v.patch - o.patch
}

// parseGoVer accepts "1.25.0", "go1.25.0", "go1.25", and
// "go version go1.24.5 darwin/amd64".
func parseGoVer(s string) (goVer, bool) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "go version ") {
		for _, f := range strings.Fields(s) {
			if strings.HasPrefix(f, "go1.") || strings.HasPrefix(f, "go2.") {
				s = f
				break
			}
		}
	}
	s = strings.TrimPrefix(s, "go")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return goVer{}, false
	}
	maj, err1 := strconv.Atoi(parts[0])
	min, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || maj < 1 {
		return goVer{}, false
	}
	pat := 0
	if len(parts) == 3 {
		var err3 error
		pat, err3 = strconv.Atoi(parts[2])
		if err3 != nil {
			return goVer{}, false
		}
	}
	return goVer{maj, min, pat}, true
}

// requiredGoVer is the higher of the go and toolchain directives.
func requiredGoVer(goMod string) (goVer, bool) {
	var req goVer
	found := false
	for _, line := range strings.Split(goMod, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[0] != "go" && fields[0] != "toolchain" {
			continue
		}
		v, ok := parseGoVer(fields[1])
		if !ok {
			continue
		}
		if !found || v.cmp(req) > 0 {
			req, found = v, true
		}
	}
	return req, found
}

// goToolchainPick is a Go toolchain to put on PATH for build.sh.
// An empty binDir means the go already on PATH is new enough.
type goToolchainPick struct {
	binDir string
	root   string
	note   string
}

func (p goToolchainPick) env() []string {
	if p.binDir == "" {
		return nil
	}
	return []string{
		"PATH=" + p.binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GOROOT=" + p.root,
	}
}

// lookupGoToolchain is the seam tests replace.
var lookupGoToolchain = ensureGoToolchain

// ensureGoToolchain returns a toolchain whose release satisfies moduleDir's
// go.mod. Services often export GOTOOLCHAIN=local so the go command will not
// download a newer toolchain itself (pipeline-46ecacd5: go.mod asked for
// 1.25.0 while this machine's go was 1.24.5). The toolchain is resolved into
// the default module cache, which persists across packs, instead of the
// throwaway GOMODCACHE build.sh creates.
func ensureGoToolchain(moduleDir string) (goToolchainPick, error) {
	data, err := os.ReadFile(filepath.Join(moduleDir, "go.mod"))
	if err != nil {
		if os.IsNotExist(err) {
			return goToolchainPick{}, nil
		}
		return goToolchainPick{}, err
	}
	req, ok := requiredGoVer(string(data))
	if !ok {
		return goToolchainPick{}, nil
	}
	cur, err := currentGoVersion()
	if err != nil {
		return goToolchainPick{}, fmt.Errorf("读取本机 Go 版本: %w", err)
	}
	if cur.cmp(req) >= 0 {
		return goToolchainPick{}, nil
	}
	root, err := downloadGoToolchain(req)
	if err != nil {
		return goToolchainPick{}, err
	}
	bin := filepath.Join(root, "bin")
	if _, err := os.Stat(filepath.Join(bin, "go")); err != nil {
		return goToolchainPick{}, fmt.Errorf("工具链 %s 缺少 bin/go: %w", root, err)
	}
	return goToolchainPick{
		binDir: bin,
		root:   root,
		note: fmt.Sprintf(
			"go.mod 需要 go%s，本机是 go%s；PATH 前置 %s（服务 build.sh 常把 GOTOOLCHAIN 设为 local，不会自己下载）",
			req, cur, bin,
		),
	}, nil
}

func currentGoVersion() (goVer, error) {
	out, err := exec.Command("go", "env", "GOVERSION").Output()
	if err != nil {
		return goVer{}, err
	}
	v, ok := parseGoVer(strings.TrimSpace(string(out)))
	if !ok {
		return goVer{}, fmt.Errorf("无法解析 %q", strings.TrimSpace(string(out)))
	}
	return v, nil
}

func downloadGoToolchain(req goVer) (string, error) {
	dir, err := os.MkdirTemp("", "acp-gotoolchain-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	body := "module acptoolchain\n\ngo " + req.String() + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(body), 0o644); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "env", "GOROOT")
	cmd.Dir = dir
	// Drop GOMODCACHE / GOTOOLCHAIN / GOROOT so the probe uses the persistent
	// default module cache and can actually switch. A parent GOTOOLCHAIN=local
	// would otherwise refuse the download.
	env := envWithout(os.Environ(), "GOMODCACHE", "GOTOOLCHAIN", "GOROOT")
	env = append(env, "GOTOOLCHAIN=go"+req.String())
	if os.Getenv("GOPROXY") == "" {
		env = append(env, "GOPROXY=https://goproxy.cn,direct")
	}
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		msg := ""
		if ee, ok := err.(*exec.ExitError); ok {
			msg = strings.TrimSpace(string(ee.Stderr))
		}
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("下载 go%s 超时: %s", req, msg)
		}
		return "", fmt.Errorf("下载 go%s: %w\n%s", req, err, msg)
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return "", fmt.Errorf("下载 go%s 后 GOROOT 为空", req)
	}
	return root, nil
}

func envWithout(base []string, keys ...string) []string {
	drop := make(map[string]bool, len(keys))
	for _, k := range keys {
		drop[k] = true
	}
	out := make([]string, 0, len(base))
	for _, kv := range base {
		k, _, ok := strings.Cut(kv, "=")
		if ok && drop[k] {
			continue
		}
		out = append(out, kv)
	}
	return out
}
