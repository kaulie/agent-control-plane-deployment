package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Host         string
	Port         int
	Home         string
	PackagesDir  string
	DataDir      string
	DBPath       string
	WebDir       string
	DeployMaxSec int
	// Max seconds for git fetch + build.sh during packageFromGit.
	ReleaseMaxSec int
	// Default max wait for project graceful restart (notify + poll).
	GracefulMaxWait time.Duration
	// Max time to wait for the project health endpoint to come back after
	// running restartCmd (polls every 2s). Avoids marking a deploy failed
	// just because the service takes a few seconds to rebind its port.
	HealthCheckTimeout time.Duration
	// Artifact storage backend: "local" (on-disk under packagesDir) or
	// "github_release" (GitHub Releases on each service's own repo). Empty
	// defaults to github_release when GITHUB_TOKEN is set, else local. Env
	// ARTIFACT_STORAGE. Future backends (S3, etc.) plug in via NewArtifactStorage.
	ArtifactStorageType string
	// GitHub token (GITHUB_TOKEN / GH_TOKEN) used to upload/download release
	// assets on each service's own repo. Empty disables release-based storage.
	GitHubToken string
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
	webDir := filepath.Join(home, "web")
	_ = os.MkdirAll(dataDir, 0o755)
	_ = os.MkdirAll(packagesDir, 0o755)
	_ = os.MkdirAll(webDir, 0o755)
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

	releaseMax := 600
	if p := os.Getenv("RELEASE_MAX_SEC"); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			releaseMax = n
		}
	}
	if releaseMax < 60 {
		releaseMax = 60
	}

	gracefulMaxWait := 10 * time.Minute
	if p := os.Getenv("GRACEFUL_RESTART_MAX_WAIT_MS"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n > 0 {
			gracefulMaxWait = time.Duration(n) * time.Millisecond
		}
	}

	healthCheckTimeout := 60 * time.Second
	if p := os.Getenv("HEALTH_CHECK_TIMEOUT_SEC"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n > 0 {
			healthCheckTimeout = time.Duration(n) * time.Second
		}
	}

	githubToken := os.Getenv("GITHUB_TOKEN")
	if githubToken == "" {
		githubToken = os.Getenv("GH_TOKEN")
	}

	artifactStorageType := strings.TrimSpace(os.Getenv("ARTIFACT_STORAGE"))

	return Config{
		Host:            host,
		Port:            port,
		Home:            home,
		PackagesDir:     packagesDir,
		DataDir:         dataDir,
		WebDir:          webDir,
		DBPath:          filepath.Join(dataDir, "deploy.sqlite"),
		DeployMaxSec:    deployMax,
		ReleaseMaxSec:   releaseMax,
		GracefulMaxWait: gracefulMaxWait,
		HealthCheckTimeout: healthCheckTimeout,
		ArtifactStorageType: artifactStorageType,
		GitHubToken:     githubToken,
	}
}
