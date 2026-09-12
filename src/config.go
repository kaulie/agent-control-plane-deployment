package main

import (
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type Config struct {
	Host         string
	Port         int
	Home         string
	PackagesDir  string
	DataDir      string
	DBPath       string
	DeployMaxSec int
	// Default max wait for project graceful restart (notify + poll).
	GracefulMaxWait time.Duration
}

func expandHome(p string) string {
	if len(p) >= 2 && p[:2] == "~/" {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

func loadConfig() Config {
	homeEnv := os.Getenv("DEPLOYMENT_HOME")
	var home string
	if homeEnv != "" {
		home = expandHome(homeEnv)
	} else {
		userHome, _ := os.UserHomeDir()
		home = filepath.Join(userHome, "runtime", "agent-control-plane-deployment")
	}
	dataDir := filepath.Join(home, "data")
	packagesDir := filepath.Join(home, "packages")
	_ = os.MkdirAll(dataDir, 0o755)
	_ = os.MkdirAll(packagesDir, 0o755)
	_ = os.MkdirAll(filepath.Join(home, "logs"), 0o755)

	host := os.Getenv("DEPLOYMENT_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	port := 4220
	if p := os.Getenv("DEPLOYMENT_PORT"); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			port = n
		}
	}
	deployMax := 120
	if p := os.Getenv("DEPLOY_MAX_SEC"); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			deployMax = n
		}
	}
	if deployMax < 30 {
		deployMax = 30
	}

	gracefulMaxWait := 10 * time.Minute
	if p := os.Getenv("GRACEFUL_RESTART_MAX_WAIT_MS"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n > 0 {
			gracefulMaxWait = time.Duration(n) * time.Millisecond
		}
	}

	return Config{
		Host:            host,
		Port:            port,
		Home:            home,
		PackagesDir:     packagesDir,
		DataDir:         dataDir,
		DBPath:          filepath.Join(dataDir, "deploy.sqlite"),
		DeployMaxSec:    deployMax,
		GracefulMaxWait: gracefulMaxWait,
	}
}
