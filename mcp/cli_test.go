package deploymcp

import (
	"os"
	"strings"
	"testing"
)

func TestPrepareCLIHelp(t *testing.T) {
	start, code, text := PrepareCLI([]string{"--help"}, strings.NewReader(""))
	if start || code != 0 {
		t.Fatalf("start=%v code=%d", start, code)
	}
	if !strings.Contains(text, "DEPLOYMENT_URL") || !strings.Contains(text, "--version") {
		t.Fatalf("usage = %s", text)
	}
}

func TestPrepareCLIVersion(t *testing.T) {
	start, code, text := PrepareCLI([]string{"--version"}, nil)
	if start || code != 0 || text != "deployment-mcp "+version+"\n" {
		t.Fatalf("start=%v code=%d text=%q", start, code, text)
	}
}

func TestPrepareCLIUnknown(t *testing.T) {
	start, code, text := PrepareCLI([]string{"--deploy"}, nil)
	if start || code != 2 || !strings.Contains(text, "未知参数") {
		t.Fatalf("start=%v code=%d text=%s", start, code, text)
	}
}

func TestSplitHTTPFlag(t *testing.T) {
	addr, rest, err := SplitHTTPFlag([]string{"--http", "127.0.0.1:4221"})
	if err != nil || addr != "127.0.0.1:4221" || len(rest) != 0 {
		t.Fatalf("addr=%q rest=%v err=%v", addr, rest, err)
	}
	if _, _, err := SplitHTTPFlag([]string{"--http"}); err == nil {
		t.Fatal("missing address should fail")
	}
}

func TestPrepareCLIPipeStartsServer(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	start, code, text := PrepareCLI(nil, r)
	if !start || code != 0 || text != "" {
		t.Fatalf("start=%v code=%d text=%q", start, code, text)
	}
}
