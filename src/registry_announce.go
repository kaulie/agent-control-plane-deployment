package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

// announceDeployment reports this process's listen address to service-registry
// after Listen has succeeded and before the server accepts requests.
//
// A disabled registry (SERVICE_REGISTRY_URL=off) skips the call and returns
// an empty id. Any other failure is returned so startup can abort: the process
// must not keep serving when the catalog has no address for it.
//
// This is one PUT .../announce, not a declarative instance sync. See
// service-registry docs/register.md.
func announceDeployment(ctx context.Context, reg *ServiceRegistry, cfg Config, port int) (string, error) {
	if !reg.Enabled() {
		log.Printf("[registry] SERVICE_REGISTRY_URL=off：跳过实例登记")
		return "", nil
	}
	spec, err := deploymentAnnounceRequest(cfg, port)
	if err != nil {
		return "", err
	}
	inst, created, err := reg.Announce(ctx, cfg.RegistryNamespace, cfg.RegistryService, spec)
	if err != nil {
		// 旧注册中心没有 PUT .../announce（404 未知路径）。地址登记失败不能挡住
		// 监听：这台机器上的面板否则永远起不来。契约缺失等其它 404 仍然中止启动。
		if announceRouteMissing(err) {
			log.Printf("[registry] 注册中心没有 /announce，跳过实例登记并继续启动：%v", err)
			return "", nil
		}
		return "", err
	}
	host := spec.Host
	if host == "" {
		host = inst.Host
	}
	if host == "" {
		host = cfg.Host
	}
	log.Printf("[registry] 已登记实例 %s/%s %s:%d os=%s arch=%s id=%s created=%v",
		cfg.RegistryNamespace, cfg.RegistryService, host, port, spec.OS, spec.Arch, inst.ID, created)
	return inst.ID, nil
}

// deregisterDeployment removes the address this process announced.
// Empty id or a disabled registry is a no-op. 404 from the registry is success.
func deregisterDeployment(ctx context.Context, reg *ServiceRegistry, cfg Config, instanceID string) error {
	if strings.TrimSpace(instanceID) == "" || !reg.Enabled() {
		return nil
	}
	if err := reg.DeleteInstance(ctx, cfg.RegistryNamespace, cfg.RegistryService, instanceID); err != nil {
		return err
	}
	log.Printf("[registry] 已摘掉实例 %s", instanceID)
	return nil
}

func deploymentAnnounceRequest(cfg Config, port int) (registryAnnounceRequest, error) {
	if port < 1 || port > 65535 {
		return registryAnnounceRequest{}, fmt.Errorf("监听端口 %d 不能登记（需要 1–65535）", port)
	}
	ns := strings.TrimSpace(cfg.RegistryNamespace)
	svc := strings.TrimSpace(cfg.RegistryService)
	if ns == "" || svc == "" {
		return registryAnnounceRequest{}, fmt.Errorf("REGISTRY_NS / REGISTRY_SERVICE 不能为空")
	}
	osName, err := registryRuntimeOS()
	if err != nil {
		return registryAnnounceRequest{}, err
	}
	return registryAnnounceRequest{
		Port: port,
		OS:   osName,
		Host: announceHost(cfg.Host),
		Arch: registryRuntimeArch(),
	}, nil
}

// registryRuntimeOS maps Go's GOOS onto the registry's os enum.
// darwin is rejected by the registry; it must be sent as mac.
func registryRuntimeOS() (string, error) {
	return registryOSName(goos())
}

func registryOSName(goosName string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(goosName)) {
	case "darwin":
		return "mac", nil
	case "linux", "mac", "windows":
		return strings.ToLower(strings.TrimSpace(goosName)), nil
	default:
		return "", fmt.Errorf("注册中心不接受 os=%q（只接受 linux / mac / windows）", goosName)
	}
}

func registryRuntimeArch() string {
	return registryArchName(goarch())
}

func registryArchName(goarchName string) string {
	switch strings.ToLower(strings.TrimSpace(goarchName)) {
	case "amd64", "x86_64":
		return "amd64"
	case "arm64", "aarch64":
		return "arm64"
	case "386":
		return "386"
	case "arm":
		return "arm"
	default:
		return ""
	}
}

// announceRouteMissing reports whether the registry process does not implement
// PUT .../announce (an older service-registry answers 404 未知路径).
func announceRouteMissing(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "未知路径") && strings.Contains(msg, "/announce")
}

// announceHost is the host field to send. A wildcard bind has no single
// address, so the field is omitted and the registry records the TCP peer
// (127.0.0.1 when this process calls the local registry).
func announceHost(listenHost string) string {
	h := strings.TrimSpace(listenHost)
	switch strings.ToLower(strings.Trim(h, "[]")) {
	case "", "0.0.0.0", "::":
		return ""
	}
	if strings.Contains(h, ":") && !strings.HasPrefix(h, "[") {
		return "[" + h + "]"
	}
	return h
}

// goos and goarch are variables so tests can cover the mapping without
// depending on the machine running the test.
var (
	goos   = func() string { return goosRuntime() }
	goarch = func() string { return goarchRuntime() }
)

func announceTimeout(cfg Config) time.Duration {
	if cfg.ServiceRegistryTimeout > 0 {
		return cfg.ServiceRegistryTimeout
	}
	return 5 * time.Second
}
