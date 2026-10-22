package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// service-registry (:4240) is the single source of truth for *which services
// exist* — its catalog is what the control plane shows and configures.
//
//	service-registry :4240 ◀──pull── this control plane :4220
//	(契约 + API + 实例，唯一真源)      (只存"部署参数"，不再自建服务)
//
// The local PostgreSQL tables keep only the deployment settings the registry does
// not model: runtimeDir / healthUrl / start-stop-restart commands / graceful
// endpoints / defaultBranch. Service ids, names, versions, owners, tags and
// gitRepoUrl come from the registry (gitRepoUrl falls back to the registry
// when the local config leaves it empty).
const defaultServiceRegistryURL = "http://127.0.0.1:4240"

// registryListLimit caps GET /v1/services. The registry itself clamps to 2000.
const registryListLimit = 1000

// RegistryEndpoint is one API endpoint of a registered service (mirrored from
// service-registry's model.Endpoint; only what the panel renders).
type RegistryEndpoint struct {
	Method  string `json:"method"`
	Path    string `json:"path"`
	Summary string `json:"summary,omitempty"`
}

// RegistryAPI is the service's public API surface (mirrored from
// service-registry's model.ServiceAPI).
type RegistryAPI struct {
	Protocols []string           `json:"protocols,omitempty"`
	DocsURL   string             `json:"docsUrl,omitempty"`
	SpecURL   string             `json:"specUrl,omitempty"`
	SpecHash  string             `json:"specHash,omitempty"`
	HasSpec   bool               `json:"hasSpec"`
	Endpoints []RegistryEndpoint `json:"endpoints,omitempty"`
}

// RegistryService is the subset of a service-registry contract we consume.
// NOTE: the registry keys services by `name`; the control plane has always
// called the same string `serviceId`.
type RegistryService struct {
	Namespace     string      `json:"namespace"`
	Name          string      `json:"name"`
	Version       string      `json:"version,omitempty"`
	Owner         string      `json:"owner,omitempty"`
	Description   string      `json:"description,omitempty"`
	GitRepoURL    string      `json:"gitRepoUrl,omitempty"`
	Tags          []string    `json:"tags,omitempty"`
	BasePath      string      `json:"basePath,omitempty"`
	HealthPath    string      `json:"healthPath,omitempty"`
	MCPPath       string      `json:"mcpPath,omitempty"` // MCP address relative to the instance root; registry default /mcp
	API           RegistryAPI `json:"api"`
	Revision      int64       `json:"revision,omitempty"`
	UpdatedAt     string      `json:"updatedAt,omitempty"`
	InstanceCount int         `json:"instanceCount"`
}

// ServiceRegistry is a read-only pull client for service-registry. A nil
// *ServiceRegistry (or SERVICE_REGISTRY_URL=off) means "not configured":
// callers must treat that as "cannot verify", never as "no services exist".
type ServiceRegistry struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewServiceRegistry returns a client, or nil when no URL is configured.
func NewServiceRegistry(cfg Config) *ServiceRegistry {
	base := strings.TrimRight(strings.TrimSpace(cfg.ServiceRegistryURL), "/")
	if base == "" {
		return nil
	}
	timeout := cfg.ServiceRegistryTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &ServiceRegistry{
		baseURL: base,
		token:   strings.TrimSpace(cfg.ServiceRegistryToken),
		client:  &http.Client{Timeout: timeout},
	}
}

// Enabled reports whether a registry URL is configured (nil-safe).
func (r *ServiceRegistry) Enabled() bool { return r != nil && r.baseURL != "" }

// BaseURL is the configured registry address (nil-safe; "" when disabled).
func (r *ServiceRegistry) BaseURL() string {
	if r == nil {
		return ""
	}
	return r.baseURL
}

// registryListBody mirrors service-registry's GET /v1/services envelope.
type registryListBody struct {
	Services []RegistryService `json:"services"`
	Total    int               `json:"total"`
}

// RegistryInstance is one service instance of the registry (only the fields that
// say *which machine it runs on* matter here).
type RegistryInstance struct {
	ID        string `json:"id,omitempty"`
	Namespace string `json:"namespace"`
	Service   string `json:"service"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	// OS 是配置中心实例上的操作系统（linux / mac）。部署前用它判断该平台的 runtime 目录是否已配好。
	OS string `json:"os,omitempty"`
	// Arch 是这台机器的 CPU 架构（amd64 / arm64 / 386 / arm）。和 OS 同时有值时才是构建平台。
	Arch     string            `json:"arch,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// registrySnapshotBody mirrors service-registry's GET /v1/snapshot envelope: the
// whole catalog — services *and* their instances — in one call. It is the single
// pull the control plane needs to answer "which machines exist".
type registrySnapshotBody struct {
	Services  []RegistryService  `json:"services"`
	Instances []RegistryInstance `json:"instances"`
}

// getJSON performs one authenticated GET against the registry and decodes it.
func (r *ServiceRegistry) getJSON(ctx context.Context, path string, out any) error {
	status, body, err := r.roundTrip(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("service_registry 返回 %d：%s", status, registryErrorMessage(body))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("解析 service_registry 响应失败：%w", err)
	}
	return nil
}

// roundTrip sends one JSON request to the registry. payload nil means no body.
// Network and configuration errors are returned; HTTP status is not — callers
// decide which codes are success.
func (r *ServiceRegistry) roundTrip(ctx context.Context, method, path string, payload any) (int, []byte, error) {
	if !r.Enabled() {
		return 0, nil, fmt.Errorf("service_registry 未配置（SERVICE_REGISTRY_URL 为空）")
	}
	var buf io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
		buf = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.baseURL+path, buf)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("请求 service_registry 失败（%s）：%w", r.baseURL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, body, nil
}

// registryAnnounceRequest is the body of PUT .../announce. Host and arch are
// omitted when empty so the registry keeps its defaults (peer address, prior arch).
type registryAnnounceRequest struct {
	Port int    `json:"port"`
	OS   string `json:"os"`
	Host string `json:"host,omitempty"`
	Arch string `json:"arch,omitempty"`
}

type registryAnnounceResponse struct {
	Instance RegistryInstance `json:"instance"`
	Created  bool             `json:"created"`
}

// Announce reports this process's listen address (PUT .../announce).
// 201 creates the instance; 200 means the same address was already registered.
// It does not remove the service's other addresses.
func (r *ServiceRegistry) Announce(ctx context.Context, namespace, service string, body registryAnnounceRequest) (RegistryInstance, bool, error) {
	path := registryServicePath(namespace, service) + "/announce"
	status, raw, err := r.roundTrip(ctx, http.MethodPut, path, body)
	if err != nil {
		return RegistryInstance{}, false, err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return RegistryInstance{}, false, fmt.Errorf("登记实例 %s/%s 失败：service_registry 返回 %d：%s",
			namespace, service, status, registryErrorMessage(raw))
	}
	var out registryAnnounceResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return RegistryInstance{}, false, fmt.Errorf("解析 service_registry 响应失败：%w", err)
	}
	if strings.TrimSpace(out.Instance.ID) == "" {
		return RegistryInstance{}, out.Created, fmt.Errorf("登记实例 %s/%s 的响应没有 instance.id", namespace, service)
	}
	return out.Instance, out.Created, nil
}

// DeleteInstance removes one announced address (DELETE .../instances/{id}).
// 404 is success: the address is already gone.
func (r *ServiceRegistry) DeleteInstance(ctx context.Context, namespace, service, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("instance id 为空")
	}
	path := registryServicePath(namespace, service) + "/instances/" + url.PathEscape(id)
	status, raw, err := r.roundTrip(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	if status == http.StatusNoContent || status == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("摘掉实例 %s 失败：service_registry 返回 %d：%s", id, status, registryErrorMessage(raw))
}

func registryServicePath(namespace, service string) string {
	return "/v1/namespaces/" + url.PathEscape(strings.TrimSpace(namespace)) +
		"/services/" + url.PathEscape(strings.TrimSpace(service))
}

// List pulls the whole service catalog from the registry.
func (r *ServiceRegistry) List(ctx context.Context) ([]RegistryService, error) {
	var out registryListBody
	if err := r.getJSON(ctx, "/v1/services?limit="+strconv.Itoa(registryListLimit), &out); err != nil {
		return nil, err
	}
	return out.Services, nil
}

// Snapshot pulls services + instances in one call (GET /v1/snapshot).
func (r *ServiceRegistry) Snapshot(ctx context.Context) ([]RegistryService, []RegistryInstance, error) {
	var out registrySnapshotBody
	if err := r.getJSON(ctx, "/v1/snapshot", &out); err != nil {
		return nil, nil, err
	}
	return out.Services, out.Instances, nil
}

// RegistryMachine 是注册中心里的一台机器：从服务实例归并出来（实例 = 服务跑在哪台机器上）。
// 机器 id 与**平台**都是实例上的字段（`metadata.machine` / `metadata.platform`），
// 控制面只做同步，不自己构造：同一个事实不在两处配置。
type RegistryMachine struct {
	ID       string
	Platform BuildPlatform // 零值 = 没登记平台
}

// MachineFromInstance 把一个服务实例归并成「机器 + 平台」。
//
//	id：`metadata.machine`（登记方给的友好名，如 gpu-2）优先，否则 host；
//	    本机（loopback / localhost / 本机 hostname）统一成 local；
//	平台：实例顶层 os + arch 都有值时用它们（mac → darwin）；否则回落
//	    `metadata.platform`（如 linux/amd64），或 `metadata.os` + `metadata.arch`。
//	只给 os、不给 arch 时不算构建平台，见 RegistryInstance.RegisteredOS。
func MachineFromInstance(inst RegistryInstance) RegistryMachine {
	out := RegistryMachine{ID: MachineIDForInstance(inst)}
	out.Platform = InstancePlatform(inst)
	return out
}

// InstancePlatform 读出实例登记的机器平台（没登记 → 零值）。
// 顶层 os 与 arch 都有合法值时优先；缺一个就不要用本机 CPU 把另一个补上。
func InstancePlatform(inst RegistryInstance) BuildPlatform {
	if p, ok := platformFromRegisteredFields(inst.OS, inst.Arch); ok {
		return p
	}
	meta := inst.Metadata
	if len(meta) == 0 {
		return BuildPlatform{}
	}
	get := func(keys ...string) string {
		for _, k := range keys {
			for mk, mv := range meta {
				if strings.EqualFold(strings.TrimSpace(mk), k) {
					return strings.TrimSpace(mv)
				}
			}
		}
		return ""
	}
	if raw := get("platform", "buildPlatform", "targetPlatform"); raw != "" {
		if p, err := ParseBuildPlatform(raw); err == nil {
			return p
		}
	}
	osName := get("os", "goos")
	arch := get("arch", "goarch")
	if osName == "" && arch == "" {
		return BuildPlatform{}
	}
	if osName == "" {
		osName = LocalBuildPlatform().OS
	}
	if p, err := ParseBuildPlatform(osName + "/" + arch); err == nil {
		return p
	}
	return BuildPlatform{}
}

// platformFromRegisteredFields 只用配置中心实例上的 os 与 arch。两者都齐才返回平台。
func platformFromRegisteredFields(osName, arch string) (BuildPlatform, bool) {
	osName = normalizeRuntimeOS(osName)
	arch = normalizeRegisteredArch(arch)
	if (osName != "linux" && osName != "darwin") || arch == "" {
		return BuildPlatform{}, false
	}
	return BuildPlatform{OS: osName, Arch: arch}, true
}

// normalizeRegisteredArch 把登记的架构收成部署平台认得的四个值。认不出就当没登记。
func normalizeRegisteredArch(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "amd64", "x86_64":
		return "amd64"
	case "arm64", "aarch64":
		return "arm64"
	case "386", "i386", "i686":
		return "386"
	case "arm", "armv7l":
		return "arm"
	default:
		return ""
	}
}

// RegisteredOS 是这台实例的操作系统（darwin / linux）。
// 配置中心的顶层 os（mac / linux）优先；老数据没有该字段时，回落到 metadata.platform 里的 OS。
func (inst RegistryInstance) RegisteredOS() (string, bool) {
	if osName := normalizeRuntimeOS(inst.OS); osName == "linux" || osName == "darwin" {
		return osName, true
	}
	if p := InstancePlatform(inst); !p.IsZero() {
		if osName := normalizeRuntimeOS(p.OS); osName == "linux" || osName == "darwin" {
			return osName, true
		}
	}
	return "", false
}

// SnapshotMachines 返回注册中心里已知的机器（含各自登记的平台）。
func (r *ServiceRegistry) SnapshotMachines(ctx context.Context) ([]RegistryMachine, error) {
	_, instances, err := r.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := []RegistryMachine{}
	for _, inst := range instances {
		m := MachineFromInstance(inst)
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// DeployMachines 只返回机器 id（老调用方/测试用）。
func (r *ServiceRegistry) DeployMachines(ctx context.Context) ([]string, error) {
	machines, err := r.SnapshotMachines(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(machines))
	for _, m := range machines {
		out = append(out, m.ID)
	}
	return out, nil
}

// MachineIDForInstance maps one registry instance to the machine it runs on.
func MachineIDForInstance(inst RegistryInstance) string {
	if isLocalHost(inst.Host) {
		return defaultDeployMachine
	}
	if named := strings.TrimSpace(inst.Metadata["machine"]); named != "" {
		return named
	}
	return strings.ToLower(strings.TrimSpace(inst.Host))
}

// isLocalHost reports whether a registry instance host is *this* machine.
func isLocalHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	switch h {
	case "", "localhost", "localhost.localdomain", "127.0.0.1", "::1", "0.0.0.0", "host.docker.internal":
		return true
	}
	if strings.HasPrefix(h, "127.") {
		return true
	}
	self, _ := os.Hostname()
	self = strings.ToLower(strings.TrimSpace(self))
	if self == "" {
		return false
	}
	return h == self || strings.HasPrefix(h, self+".") || strings.HasPrefix(self, h+".")
}

// Lookup returns the registered contract for serviceID. found=false (with a
// nil error) means the registry answered and does not know this service.
func (r *ServiceRegistry) Lookup(ctx context.Context, serviceID string) (*RegistryService, bool, error) {
	svcs, err := r.List(ctx)
	if err != nil {
		return nil, false, err
	}
	for i := range svcs {
		if svcs[i].Name == serviceID {
			return &svcs[i], true, nil
		}
	}
	return nil, false, nil
}

// GitRepoURL returns the registered code repo for serviceID ("" when the
// service is unknown or has no repo registered).
func (r *ServiceRegistry) GitRepoURL(ctx context.Context, serviceID string) (string, error) {
	svc, found, err := r.Lookup(ctx, serviceID)
	if err != nil || !found {
		return "", err
	}
	return strings.TrimSpace(svc.GitRepoURL), nil
}

// registryErrorMessage extracts {"error":{"code","message"}} if present.
func registryErrorMessage(body []byte) string {
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Error.Message != "" {
		if env.Error.Code != "" {
			return env.Error.Code + ": " + env.Error.Message
		}
		return env.Error.Message
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	if msg == "" {
		msg = "空响应"
	}
	return msg
}

// ServiceCatalogEntry is one row of the 服务契约 tab: the registry's contract
// merged with this control plane's deployment config (embedded flat, so the
// panel keeps reading serviceId/runtimeDir/healthUrl/... as before).
type ServiceCatalogEntry struct {
	ServiceContract
	// Registered: the service exists in service_registry — the only way a
	// service can appear here, since the control plane cannot create one.
	Registered bool `json:"registered"`
	// Configured: this control plane has deployment settings for it.
	Configured bool `json:"configured"`
	// Registry: the registry's metadata (nil when registered=false).
	Registry *RegistryService `json:"registry,omitempty"`
}

// RegistryStatus describes the registry's availability for one catalog read,
// so the panel can say where the list came from / that the pull failed
// instead of silently showing a short list.
type RegistryStatus struct {
	URL      string `json:"url,omitempty"`
	Enabled  bool   `json:"enabled"`
	OK       bool   `json:"ok"`
	Services int    `json:"services"`
	Error    string `json:"error,omitempty"`
}

// buildServiceCatalog merges the registry catalog (source of truth for which
// services exist) with the local deployment config:
//   - registry service + local config   → Registered + Configured (编辑配置)
//   - registry service, no local config → Registered, needs 配置
//   - local config the registry did not return (registry down, or the service
//     has not been registered yet) → Registered=false, so rows never silently
//     disappear and existing deploys keep working.
func buildServiceCatalog(ctx context.Context, store *Store, reg *ServiceRegistry) ([]ServiceCatalogEntry, RegistryStatus, error) {
	locals, err := store.ListServices()
	if err != nil {
		return nil, RegistryStatus{}, err
	}
	localByID := make(map[string]ServiceContract, len(locals))
	for _, s := range locals {
		localByID[s.ServiceID] = s
	}

	status := RegistryStatus{URL: reg.BaseURL(), Enabled: reg.Enabled()}
	entries := make([]ServiceCatalogEntry, 0, len(locals))
	seen := make(map[string]bool, len(locals))
	if reg.Enabled() {
		svcs, err := reg.List(ctx)
		if err != nil {
			status.Error = err.Error()
		} else {
			status.OK = true
			status.Services = len(svcs)
			for i := range svcs {
				rs := svcs[i]
				if rs.Name == "" {
					continue
				}
				entry := ServiceCatalogEntry{
					ServiceContract: ServiceContract{ServiceID: rs.Name},
					Registered:      true,
					Registry:        &rs,
				}
				if local, ok := localByID[rs.Name]; ok {
					entry.ServiceContract = local
					entry.Configured = true
					// 本机还没填健康检查路径时，用注册中心登记的 healthPath（API 面在注册中心，
					// 路径同样与机器无关）。保存时它会落到契约里。
					if strings.TrimSpace(entry.HealthURL) == "" {
						if hp := strings.TrimSpace(rs.HealthPath); hp != "" {
							entry.HealthURL = ensureLeadingSlash(hp)
						}
					}
				}
				entries = append(entries, entry)
				seen[rs.Name] = true
			}
		}
	}

	rest := make([]ServiceContract, 0, len(locals))
	for _, s := range locals {
		if !seen[s.ServiceID] {
			rest = append(rest, s)
		}
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i].ServiceID < rest[j].ServiceID })
	for _, s := range rest {
		entries = append(entries, ServiceCatalogEntry{ServiceContract: s, Registered: false, Configured: true})
	}
	return entries, status, nil
}

// resolveServiceGitRepo returns the repo to package a service from.
// gitRepoUrl 是 service_registry 同步过来的元信息 —— 注册中心是唯一真源，
// 本机不能改（PUT 显式改动会被拒）。只有注册中心不可用/未登记这个字段时，
// 才退回本机镜像的旧值；两边都没有 → ""（调用方给出明确报错）。
func resolveServiceGitRepo(ctx context.Context, reg *ServiceRegistry, local *ServiceContract) string {
	if local == nil {
		return ""
	}
	if reg.Enabled() {
		if u, err := reg.GitRepoURL(ctx, local.ServiceID); err == nil && strings.TrimSpace(u) != "" {
			return strings.TrimSpace(u)
		}
	}
	return strings.TrimSpace(local.GitRepoURL)
}
