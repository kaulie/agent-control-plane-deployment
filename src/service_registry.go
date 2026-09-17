package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
// The local SQLite table keeps only the deployment settings the registry does
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

// List pulls the whole service catalog from the registry.
func (r *ServiceRegistry) List(ctx context.Context) ([]RegistryService, error) {
	if !r.Enabled() {
		return nil, fmt.Errorf("service_registry 未配置（SERVICE_REGISTRY_URL 为空）")
	}
	endpoint := r.baseURL + "/v1/services?limit=" + strconv.Itoa(registryListLimit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 service_registry 失败（%s）：%w", r.baseURL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("service_registry 返回 %d：%s", resp.StatusCode, registryErrorMessage(body))
	}
	var out registryListBody
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("解析 service_registry 响应失败：%w", err)
	}
	return out.Services, nil
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
