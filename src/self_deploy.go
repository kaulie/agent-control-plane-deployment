package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Self-upgrade hand-off: deployment stages artifacts, then drops a request
// for the independent acp-upgrader process (no in-process restart / spawn).

type acpUpgradeRequest struct {
	RequestID  string `json:"requestId"`
	ServiceID  string `json:"serviceId"`
	Deployment string `json:"deployment"`
	Version    string `json:"version"`
	Home       string `json:"home"`
	HealthURL  string `json:"healthUrl"`
	StagedAt   string `json:"stagedAt"`
}

func isSelfDeploy(service ServiceContract, cfg Config) bool {
	a, err1 := filepath.Abs(filepath.Clean(service.RuntimeDir))
	b, err2 := filepath.Abs(filepath.Clean(cfg.Home))
	if err1 != nil || err2 != nil {
		return filepath.Clean(service.RuntimeDir) == filepath.Clean(cfg.Home)
	}
	return a == b
}

func upgraderPidPath(home string) string {
	return filepath.Join(home, "upgrader.pid")
}

func upgraderRunning(home string) bool {
	raw, err := os.ReadFile(upgraderPidPath(home))
	if err != nil {
		return false
	}
	pidStr := strings.TrimSpace(string(raw))
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid <= 0 {
		return false
	}
	err = syscall.Kill(pid, syscall.Signal(0))
	return err == nil
}

func enqueueACPUpgrade(cfg Config, job DeployJob, service ServiceContract, version, deployment string) error {
	dir := filepath.Join(cfg.Home, "upgrade-requests")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	req := acpUpgradeRequest{
		RequestID:  job.RequestID,
		ServiceID:  service.ServiceID,
		Deployment: deployment,
		Version:    version,
		Home:       cfg.Home,
		HealthURL:  service.HealthURL,
		StagedAt:   time.Now().UTC().Format(time.RFC3339Nano),
	}
	raw, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, job.RequestID+".json.tmp")
	final := filepath.Join(dir, job.RequestID+".json")
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

func selfDeployRsyncCmd(src, dest string) string {
	return strings.Join([]string{
		"rsync", "-a", "--delete",
		"--filter='P packages/'",
		"--filter='P data/'",
		"--filter='P logs/'",
		"--filter='P deployment.pid'",
		"--filter='P upgrader.pid'",
		"--filter='P upgrade-requests/'",
		"--exclude='packages/'",
		"--exclude='data/'",
		"--exclude='logs/'",
		"--exclude='deployment.pid'",
		"--exclude='upgrader.pid'",
		"--exclude='upgrade-requests/'",
		"--exclude='.git/'",
		fmt.Sprintf("%q", src+"/"),
		fmt.Sprintf("%q", dest+"/"),
	}, " ")
}
