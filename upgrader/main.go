package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// UpgradeRequest is dropped by deployment-server after artifacts are staged.
// acp-upgrader never builds; it only stop → start the already-prepared runtime.
type UpgradeRequest struct {
	RequestID  string `json:"requestId"`
	ServiceID  string `json:"serviceId"`
	Deployment string `json:"deployment"`
	Version    string `json:"version"`
	Home       string `json:"home"`
	HealthURL  string `json:"healthUrl"`
	StagedAt   string `json:"stagedAt"`
}

func main() {
	home := os.Getenv("DEPLOYMENT_HOME")
	if home == "" {
		userHome, _ := os.UserHomeDir()
		home = filepath.Join(userHome, "runtime", "agent-control-plane-deployment")
	}
	reqDir := filepath.Join(home, "upgrade-requests")
	_ = os.MkdirAll(reqDir, 0o755)
	_ = os.MkdirAll(filepath.Join(home, "logs"), 0o755)

	logf("acp-upgrader watching %s (home=%s)", reqDir, home)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			logf("shutting down")
			return
		case <-ticker.C:
			if err := processOnce(home, reqDir); err != nil {
				logf("process error: %v", err)
			}
		}
	}
}

func processOnce(home, reqDir string) error {
	entries, err := os.ReadDir(reqDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(reqDir, name)
		if err := handleRequest(home, path); err != nil {
			logf("request %s failed: %v", name, err)
			_ = os.WriteFile(path+".failed", []byte(err.Error()+"\n"), 0o644)
			_ = os.Rename(path, path+".bad")
			continue
		}
	}
	return nil
}

func handleRequest(home, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var req UpgradeRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return fmt.Errorf("json: %w", err)
	}
	if req.RequestID == "" {
		return fmt.Errorf("requestId required")
	}
	if req.Home != "" {
		home = req.Home
	}
	healthURL := strings.TrimSpace(req.HealthURL)
	if healthURL == "" {
		healthURL = "http://127.0.0.1:4220/health"
	}

	bin := filepath.Join(home, "bin", "deployment-server")
	if st, err := os.Stat(bin); err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
		return fmt.Errorf("prepared artifact missing or not executable: %s", bin)
	}

	logf("activating requestId=%s version=%s deployment=%s", req.RequestID, req.Version, req.Deployment)

	stopSh := filepath.Join(home, "scripts", "stop.sh")
	startSh := filepath.Join(home, "scripts", "start.sh")
	if err := runBash(stopSh, home); err != nil {
		logf("stop: %v (continuing)", err)
	}
	time.Sleep(500 * time.Millisecond)
	if err := runBash(startSh, home); err != nil {
		return fmt.Errorf("start: %w", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if healthOK(healthURL) {
			logf("ok requestId=%s health=%s", req.RequestID, healthURL)
			done := path + ".done"
			_ = os.WriteFile(done, []byte(fmt.Sprintf("ok %s\n", time.Now().UTC().Format(time.RFC3339))), 0o644)
			_ = os.Remove(path)
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("health check failed after start: %s", healthURL)
}

func runBash(script, home string) error {
	cmd := exec.Command("/bin/bash", script)
	cmd.Dir = home
	cmd.Env = append(os.Environ(),
		"DEPLOYMENT_HOME="+home,
		"DEPLOYMENT_PORT=4220",
		"PORT=4220",
		"HOST=127.0.0.1",
	)
	out, err := cmd.CombinedOutput()
	if len(out) > 0 {
		logf("%s", strings.TrimSpace(string(out)))
	}
	return err
}

func healthOK(rawURL string) bool {
	client := &http.Client{Timeout: 3 * time.Second}
	res, err := client.Get(rawURL)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	return res.StatusCode >= 200 && res.StatusCode < 300
}

func logf(format string, args ...any) {
	fmt.Printf("[acp-upgrader] "+format+"\n", args...)
}
