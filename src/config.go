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
	// Artifact storage backend: "local" (on-disk under packagesDir),
	// "github_release" (GitHub Releases on each service's own repo) or
	// "aliyun" (Aliyun packages generic repo). Empty defaults to
	// github_release when GITHUB_TOKEN is set, else local. Env
	// ARTIFACT_STORAGE. Future backends (S3, etc.) plug in via
	// NewArtifactStorage.
	ArtifactStorageType string
	// GitHub token (GITHUB_TOKEN / GH_TOKEN) used to upload/download release
	// assets on each service's own repo. Empty disables release-based storage.
	GitHubToken string
	// Aliyun packages (制品仓库) generic-repo settings for the "aliyun"
	// backend. baseURL/productID/repo default to the deployment control
	// plane's repo; credentials (ALIYUN_PACKAGES_USER / ALIYUN_PACKAGES_PASSWORD)
	// are required and never hardcoded.
	AliyunBaseURL   string
	AliyunProductID string
	AliyunRepo      string
	AliyunUsername  string
	AliyunPassword  string
	// Require the phase-1 identity headers (identity_role / identity_id) on the
	// deployment-triggering APIs. Default true; IDENTITY_ENFORCE=0 turns the
	// check into log-only (deploys are then recorded as unidentified) — a
	// rollback hatch while every caller is migrated.
	IdentityEnforce bool
	// service-registry (:4240) base URL. The service catalog shown/configured
	// by the panel is pulled from here (服务列表统一从注册中心拉取), and
	// gitRepoUrl falls back to it. Empty = disabled (SERVICE_REGISTRY_URL=off);
	// the control plane then only shows its locally configured services.
	ServiceRegistryURL string
	// Optional bearer token for the registry (REGISTRY_READ_AUTH=token).
	ServiceRegistryToken string
	// Per-request timeout for registry pulls.
	ServiceRegistryTimeout time.Duration
	// Where the「打包走本机代理」option reads the local proxy from
	// (PROXY_ENV_FILE; default <home>/data/proxy.env, the file the ops scripts
	// use). "off" disables the option entirely; a missing file falls back to
	// the service process' own HTTP(S)_PROXY.
	ProxyEnvFile string
	// Known deployment machines (DEPLOY_MACHINES, comma-separated ids): **发现**用，
	// 让面板/事件知道还有哪些机器（例如注册中心实例主机）。真正能不能被选中取决于
	// 有没有 DeployMachineTargets（部署通道）。
	DeployMachines []string
	// DeployMachineTargets 是「怎么把部署送到某台机器」（DEPLOY_MACHINE_TARGETS，
	// 或本机 data/machine-targets）：有通道的机器才可选、才会真的部署到那台机器。
	DeployMachineTargets map[string]MachineTarget
	// 通道配置解析失败的原因（进 /api/meta 提示，不静默忽略）。
	MachineTargetsError string
	// Machine used when a trigger does not pick one (DEPLOY_DEFAULT_MACHINE).
	// Empty = the first configured machine.
	DefaultDeployMachine string
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

	// Aliyun packages (制品仓库) backend. baseURL/productID/repo are non-secret
	// and default to the control plane's own generic repo; credentials have no
	// defaults and must be supplied (ALIYUN_PACKAGES_USER / ALIYUN_PACKAGES_PASSWORD).
	aliyunBase := strings.TrimSpace(os.Getenv("ALIYUN_PACKAGES_BASE_URL"))
	if aliyunBase == "" {
		aliyunBase = defaultAliyunBaseURL
	}
	aliyunProduct := strings.TrimSpace(os.Getenv("ALIYUN_PACKAGES_PRODUCT_ID"))
	if aliyunProduct == "" {
		aliyunProduct = defaultAliyunProductID
	}
	aliyunRepo := strings.TrimSpace(os.Getenv("ALIYUN_PACKAGES_REPO"))
	if aliyunRepo == "" {
		aliyunRepo = defaultAliyunRepo
	}

	// Identity enforcement on deploy APIs: on unless explicitly disabled.
	identityEnforce := true
	switch strings.ToLower(strings.TrimSpace(os.Getenv("IDENTITY_ENFORCE"))) {
	case "0", "false", "no", "off":
		identityEnforce = false
	}

	// service-registry (:4240) — the source of the service catalog. "off" /
	// "disabled" turns the pull off explicitly (then only locally configured
	// services are shown; nothing is auto-created either).
	registryURL := strings.TrimSpace(os.Getenv("SERVICE_REGISTRY_URL"))
	switch strings.ToLower(registryURL) {
	case "off", "disabled", "none":
		registryURL = ""
	case "":
		registryURL = defaultServiceRegistryURL
	}
	registryTimeout := 5 * time.Second
	if p := os.Getenv("SERVICE_REGISTRY_TIMEOUT_SEC"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n > 0 {
			registryTimeout = time.Duration(n) * time.Second
		}
	}

	// 本机代理（发起流水线时可勾选「走本机代理」）。默认读运维脚本同一份文件
	// data/proxy.env；PROXY_ENV_FILE=off 关掉这个选项。
	// 部署机器通道（DEPLOY_MACHINE_TARGETS）：解析失败不致命（服务照常起），
	// 但原因要带到 /api/meta 的提示里。
	machineTargets, machineTargetsErr := ParseMachineTargets(os.Getenv(deployMachineTargetsEnv))
	machineTargetsError := ""
	if machineTargetsErr != nil {
		machineTargetsError = machineTargetsErr.Error()
		machineTargets = map[string]MachineTarget{defaultDeployMachine: {ID: defaultDeployMachine, Kind: "local"}}
	}

	proxyEnvFile := strings.TrimSpace(os.Getenv("PROXY_ENV_FILE"))
	if proxyEnvFile == "" {
		proxyEnvFile = filepath.Join(dataDir, "proxy.env")
	}

	// 部署机器（发起流水线时可选择）：DEPLOY_MACHINES 逗号分隔；缺省单机 "local"。
	// DEPLOY_DEFAULT_MACHINE 指定默认机器（未选时用），缺省用列表第一台。
	deployMachines := parseDeployMachines(os.Getenv("DEPLOY_MACHINES"))
	if len(deployMachines) == 0 {
		deployMachines = []string{defaultDeployMachine}
	}
	defaultDeployMachine := strings.TrimSpace(os.Getenv("DEPLOY_DEFAULT_MACHINE"))
	if defaultDeployMachine == "" {
		defaultDeployMachine = deployMachines[0]
	}

	return Config{
		Host:                   host,
		Port:                   port,
		Home:                   home,
		PackagesDir:            packagesDir,
		DataDir:                dataDir,
		WebDir:                 webDir,
		DBPath:                 filepath.Join(dataDir, "deploy.sqlite"),
		DeployMaxSec:           deployMax,
		ReleaseMaxSec:          releaseMax,
		GracefulMaxWait:        gracefulMaxWait,
		HealthCheckTimeout:     healthCheckTimeout,
		ArtifactStorageType:    artifactStorageType,
		GitHubToken:            githubToken,
		AliyunBaseURL:          aliyunBase,
		AliyunProductID:        aliyunProduct,
		AliyunRepo:             aliyunRepo,
		AliyunUsername:         os.Getenv("ALIYUN_PACKAGES_USER"),
		AliyunPassword:         os.Getenv("ALIYUN_PACKAGES_PASSWORD"),
		IdentityEnforce:        identityEnforce,
		ServiceRegistryURL:     registryURL,
		ServiceRegistryToken:   strings.TrimSpace(os.Getenv("SERVICE_REGISTRY_TOKEN")),
		ServiceRegistryTimeout: registryTimeout,
		ProxyEnvFile:           proxyEnvFile,
		DeployMachines:         deployMachines,
		DefaultDeployMachine:   defaultDeployMachine,
		DeployMachineTargets:   machineTargets,
		MachineTargetsError:    machineTargetsError,
	}
}
