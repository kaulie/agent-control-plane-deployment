package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/kaulie/agent-control-plane-deployment/eventlevel"
)

type apiServer struct {
	store    *Store
	cfg      Config
	storage  ArtifactStorage
	worker   *DeployWorker
	pipeline *PipelineWorker
	drain    *GracefulDrain
	// registry is the read-only service-registry pull client (nil = not
	// configured). The service catalog is its data; local rows are only
	// deployment config.
	registry *ServiceRegistry
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *apiServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /api/services", s.handleListServices)
	mux.HandleFunc("GET /api/services/{serviceId}", s.handleGetService)
	mux.HandleFunc("PUT /api/services/{serviceId}", s.handlePutService)
	mux.HandleFunc("DELETE /api/services/{serviceId}", s.handleDeleteService)
	mux.HandleFunc("POST /api/deploys", s.handleCreateDeploy)
	mux.HandleFunc("GET /api/deploys", s.handleListDeploys)
	mux.HandleFunc("GET /api/deploys/{requestId}", s.handleGetDeploy)
	mux.HandleFunc("GET /api/deploys/{requestId}/events", s.handleListDeployEvents)
	mux.HandleFunc("POST /api/deploy-notify", s.handleDeployNotify)
	mux.HandleFunc("GET /api/pipelines", s.handleListPipelines)
	mux.HandleFunc("GET /api/pipelines/{requestId}", s.handleGetPipeline)
	mux.HandleFunc("GET /api/pipelines/{requestId}/events", s.handleListPipelineEvents)
	mux.HandleFunc("GET /api/artifacts", s.handleListArtifacts)
	mux.HandleFunc("GET /api/artifacts/{tag}", s.handleGetArtifact)
	mux.HandleFunc("POST /api/artifacts/scan", s.handleScanArtifacts)
	mux.HandleFunc("POST /restart/notify", s.handleRestartNotify)
	mux.HandleFunc("GET /restart/poll", s.handleRestartPoll)
	mux.HandleFunc("GET /api/meta", s.handleMeta)
	s.registerPanel(mux)
	return withCORS(mux)
}

// registerPanel serves the standalone web panel (vanilla HTML/CSS/JS) from
// cfg.WebDir on the same port as the API. The panel lives under /panel/ so it
// never shadows /health or /api/*. Root "/" redirects to /panel/.
func (s *apiServer) registerPanel(mux *http.ServeMux) {
	webDir := s.cfg.WebDir
	if webDir == "" {
		return
	}
	fs := http.FileServer(http.Dir(webDir))
	mux.Handle("GET /panel/", http.StripPrefix("/panel/", fs))
	mux.HandleFunc("GET /panel", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/panel/", http.StatusFound)
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/panel/", http.StatusFound)
	})
}

func (s *apiServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"service": "agent-control-plane-deployment",
		"home":    s.cfg.Home,
		"time":    nowISO(),
	})
}

// handleListServices returns the service catalog: the service-registry's
// contracts (the single source of truth for which services exist) merged with
// this control plane's deployment config. `registry` in the response tells the
// panel whether the pull succeeded, so a registry outage is visible instead of
// looking like "no services".
func (s *apiServer) handleListServices(w http.ResponseWriter, r *http.Request) {
	services, status, err := buildServiceCatalog(r.Context(), s.store, s.registry)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"services": services, "registry": status})
}

// handleGetService returns one merged catalog entry (404 when the service is
// neither registered nor locally configured).
func (s *apiServer) handleGetService(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("serviceId"))
	services, _, err := buildServiceCatalog(r.Context(), s.store, s.registry)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, entry := range services {
		if entry.ServiceID == id {
			writeJSON(w, http.StatusOK, entry)
			return
		}
	}
	writeError(w, http.StatusNotFound, "service not found: "+id)
}

type putServiceBody struct {
	Name              string  `json:"name"`
	RuntimeDir        string  `json:"runtimeDir"`
	HealthURL         string  `json:"healthUrl"`
	Port              *int    `json:"port"`
	StartCmd          string  `json:"startCmd"`
	StopCmd           string  `json:"stopCmd"`
	RestartCmd        string  `json:"restartCmd"`
	GitRepoURL        *string `json:"gitRepoUrl"`
	DefaultBranch     *string `json:"defaultBranch"`
	RestartNotifyURL  *string `json:"restartNotifyUrl"`
	RestartPollURL    *string `json:"restartPollUrl"`
	GracefulMaxWaitMs *int    `json:"gracefulRestartMaxWaitMs"`
}

func (s *apiServer) handlePutService(w http.ResponseWriter, r *http.Request) {
	serviceID := strings.TrimSpace(r.PathValue("serviceId"))
	if serviceID == "" {
		writeError(w, http.StatusBadRequest, "serviceId required")
		return
	}
	var body putServiceBody
	_ = json.NewDecoder(r.Body).Decode(&body)

	existing, err := s.store.GetService(serviceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 服务契约不能在本机新建：服务是否存在由 service_registry 说了算。只有
	// "已登记的服务"才能在这里配置部署参数；已有本地配置的照旧可改（注册中心
	// 不可用也不能把运维锁死）。
	var registered *RegistryService
	var registryErr error
	if s.registry.Enabled() {
		if svc, found, lookupErr := s.registry.Lookup(r.Context(), serviceID); lookupErr != nil {
			registryErr = lookupErr
		} else if found {
			registered = svc
		}
	}
	if existing == nil {
		if !s.registry.Enabled() {
			writeError(w, http.StatusServiceUnavailable,
				"service_registry 未配置（SERVICE_REGISTRY_URL=off），本机不能新建服务契约")
			return
		}
		if registryErr != nil {
			writeError(w, http.StatusServiceUnavailable,
				"无法确认服务是否已在 service_registry 登记："+registryErr.Error())
			return
		}
		if registered == nil {
			writeError(w, http.StatusBadRequest,
				`service "`+serviceID+`" 未在 service_registry 中登记；服务列表统一从注册中心拉取，`+
					`本机只能配置已登记服务的部署参数（请先在注册中心登记该服务）`)
			return
		}
	}

	name := strings.TrimSpace(body.Name)
	runtimeDir := strings.TrimSpace(body.RuntimeDir)
	healthURL := strings.TrimSpace(body.HealthURL)
	port := 0
	startCmd := strings.TrimSpace(body.StartCmd)
	stopCmd := strings.TrimSpace(body.StopCmd)
	restartCmd := strings.TrimSpace(body.RestartCmd)
	notifyURL := ""
	pollURL := ""
	maxWaitMs := 0
	gitRepoURL := ""
	defaultBranch := "main"
	if existing != nil {
		if name == "" {
			name = existing.Name
		}
		if runtimeDir == "" {
			runtimeDir = existing.RuntimeDir
		}
		if healthURL == "" {
			healthURL = existing.HealthURL
		}
		if startCmd == "" {
			startCmd = existing.StartCmd
		}
		if stopCmd == "" {
			stopCmd = existing.StopCmd
		}
		if restartCmd == "" {
			restartCmd = existing.RestartCmd
		}
		notifyURL = existing.RestartNotifyURL
		pollURL = existing.RestartPollURL
		maxWaitMs = existing.GracefulMaxWaitMs
		gitRepoURL = existing.GitRepoURL
		port = existing.Port
		defaultBranch = defaultBranchOrMain(existing.DefaultBranch)
	}
	// 服务端口：**必填**（1..65535）。它会在启动/停止/重启时注入 SERVICE_PORT。
	// 缺省（不传）时沿用库里已有的端口；库里也没有（老契约 port=0）→ 400。
	if body.Port != nil {
		p := *body.Port
		if p < 1 || p > 65535 {
			writeError(w, http.StatusBadRequest,
				"port 必须指定且在 1..65535 之间（服务启动时会注入 SERVICE_PORT）")
			return
		}
		port = p
	}
	if normalizePort(port) == 0 {
		writeError(w, http.StatusBadRequest,
			"port 必须指定（1..65535）：服务启动时会注入 SERVICE_PORT，不再按 healthUrl 猜端口")
		return
	}
	// 服务端口必须唯一：同一个端口不能被两个服务用（否则后起的服务起不来）。
	if holder, err := s.store.ServiceByPort(port, serviceID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	} else if holder != nil {
		writeError(w, http.StatusConflict,
			fmt.Sprintf("端口 %d 已被服务 %q 占用；服务端口必须唯一，请换一个", port, holder.ServiceID))
		return
	}
	if body.RestartNotifyURL != nil {
		notifyURL = strings.TrimSpace(*body.RestartNotifyURL)
	}
	if body.RestartPollURL != nil {
		pollURL = strings.TrimSpace(*body.RestartPollURL)
	}
	if body.GracefulMaxWaitMs != nil {
		maxWaitMs = *body.GracefulMaxWaitMs
		if maxWaitMs < 0 {
			maxWaitMs = 0
		}
	}
	// gitRepoUrl 是 service_registry 同步过来的元信息：本机不能改。显式改成别的
	// 值直接报错（"以为改了其实没改"更糟），其余情况一律以注册中心为真源；注册
	// 中心没登记这个字段时保留本机镜像的旧值（旧数据仍然可部署）。
	registryGitRepo := ""
	if registered != nil {
		registryGitRepo = strings.TrimSpace(registered.GitRepoURL)
	}
	if body.GitRepoURL != nil {
		want := strings.TrimSpace(*body.GitRepoURL)
		allowed := registryGitRepo
		if registered == nil {
			if existing != nil {
				allowed = strings.TrimSpace(existing.GitRepoURL)
			} else {
				allowed = ""
			}
		}
		if want != allowed {
			writeError(w, http.StatusBadRequest,
				"gitRepoUrl 来自 service_registry，本机不能修改（当前登记值："+allowed+
					"）；请在注册中心更新")
			return
		}
	}
	if registryGitRepo != "" {
		gitRepoURL = registryGitRepo
	}
	if body.DefaultBranch != nil {
		defaultBranch = defaultBranchOrMain(*body.DefaultBranch)
	}
	if name == "" {
		name = serviceID
	}
	if runtimeDir == "" || healthURL == "" || startCmd == "" || stopCmd == "" || restartCmd == "" {
		writeError(w, http.StatusBadRequest,
			"runtimeDir, healthUrl, startCmd, stopCmd, restartCmd are required (or update an existing service)")
		return
	}
	// Graceful restart is all-or-nothing: both URLs or neither.
	if (notifyURL == "") != (pollURL == "") {
		writeError(w, http.StatusBadRequest,
			"restartNotifyUrl and restartPollUrl must both be set (graceful) or both empty (direct restart)")
		return
	}

	svc, err := s.store.UpsertService(ServiceContract{
		ServiceID:         serviceID,
		Name:              name,
		RuntimeDir:        runtimeDir,
		HealthURL:         healthURL,
		Port:              port,
		StartCmd:          startCmd,
		StopCmd:           stopCmd,
		RestartCmd:        restartCmd,
		GitRepoURL:        gitRepoURL,
		DefaultBranch:     defaultBranch,
		RestartNotifyURL:  notifyURL,
		RestartPollURL:    pollURL,
		GracefulMaxWaitMs: maxWaitMs,
	})
	if err != nil {
		// 并发保存时可能绕过上面的检查、撞到 services.port 的唯一索引：同样给友好的 409。
		if isServicePortConflict(err) {
			writeError(w, http.StatusConflict,
				fmt.Sprintf("端口 %d 已被其它服务占用；服务端口必须唯一，请换一个", port))
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	status := http.StatusCreated
	if existing != nil {
		status = http.StatusOK
	}
	writeJSON(w, status, svc)
}

// isServicePortConflict 识别 services.port 唯一约束被触发（并发写入的兜底路径）。
func isServicePortConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique") && strings.Contains(msg, "services.port")
}

func (s *apiServer) handleDeleteService(w http.ResponseWriter, r *http.Request) {
	ok, err := s.store.DeleteService(r.PathValue("serviceId"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "service not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type createDeployBody struct {
	ServiceID  string `json:"serviceId"`
	Deployment string `json:"deployment"`
	Hash       string `json:"hash"`
	RequestID  string `json:"requestId"`
}

func (s *apiServer) handleCreateDeploy(w http.ResponseWriter, r *http.Request) {
	var body createDeployBody
	_ = json.NewDecoder(r.Body).Decode(&body)
	// Phase-1 identity: the caller must identify itself (identity_role /
	// identity_id) so the deploy is attributable.
	by, ok := s.requireIdentity(w, r)
	if !ok {
		return
	}
	serviceID := strings.TrimSpace(body.ServiceID)
	raw := strings.TrimSpace(body.Deployment)
	if raw == "" {
		raw = strings.TrimSpace(body.Hash)
	}
	if serviceID == "" {
		writeError(w, http.StatusBadRequest, "serviceId is required")
		return
	}
	if raw == "" {
		writeError(w, http.StatusBadRequest, "deployment or hash is required")
		return
	}
	svc, err := s.store.GetService(serviceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if svc == nil {
		writeError(w, http.StatusNotFound, "service not found: "+serviceID)
		return
	}
	deployment, err := assertRelease(s.storage, serviceID, resolveServiceGitRepo(r.Context(), s.registry, svc), raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	requestID := strings.TrimSpace(body.RequestID)
	if requestID == "" {
		requestID = "deploy-req-" + uuid.NewString()[:8]
	}
	if existing, _ := s.store.GetDeploy(requestID); existing != nil {
		writeError(w, http.StatusConflict, "request already exists: "+requestID)
		return
	}
	job, err := s.store.CreateDeploy(requestID, serviceID, deployment, by, "queued for deployment worker")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	fmt.Printf("[deploy] %s service=%s deployment=%s by=%s\n",
		requestID, serviceID, deployment, by.String())
	if s.worker != nil {
		s.worker.Kick()
	}

	type resp struct {
		DeployJob
		Poll string `json:"poll"`
	}
	writeJSON(w, http.StatusAccepted, resp{DeployJob: job, Poll: "/api/deploys/" + requestID})
}

func (s *apiServer) handleListDeploys(w http.ResponseWriter, r *http.Request) {
	f := parseListFilter(r)
	deploys, total, err := s.store.ListDeploysFiltered(f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deploys": deploys,
		"total":   total,
		"page":    f.Page,
		"pageSize": f.PageSize,
	})
}

func (s *apiServer) handleGetDeploy(w http.ResponseWriter, r *http.Request) {
	job, err := s.store.GetDeploy(r.PathValue("requestId"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if job == nil {
		writeError(w, http.StatusNotFound, "deploy not found")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *apiServer) handleListDeployEvents(w http.ResponseWriter, r *http.Request) {
	requestID := r.PathValue("requestId")
	job, err := s.store.GetDeploy(requestID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if job == nil {
		writeError(w, http.StatusNotFound, "deploy not found")
		return
	}
	events, err := s.store.ListDeployEvents(requestID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

type deployNotifyBody struct {
	ServiceID string `json:"serviceId"`
	Ref       string `json:"ref"`
	RequestID string `json:"requestId"`
}

// POST /api/deploy-notify — service asks ACP to package then deploy (graceful).
func (s *apiServer) handleDeployNotify(w http.ResponseWriter, r *http.Request) {
	var body deployNotifyBody
	_ = json.NewDecoder(r.Body).Decode(&body)
	// Phase-1 identity: the caller must identify itself so the pipeline (and the
	// deploy it enqueues) is attributable in the panel/audit trail.
	by, ok := s.requireIdentity(w, r)
	if !ok {
		return
	}
	serviceID := strings.TrimSpace(body.ServiceID)
	if serviceID == "" {
		writeError(w, http.StatusBadRequest, "serviceId is required")
		return
	}
	svc, err := s.store.GetService(serviceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if svc == nil {
		writeError(w, http.StatusNotFound, "service not found: "+serviceID)
		return
	}
	if resolveServiceGitRepo(r.Context(), s.registry, svc) == "" {
		writeError(w, http.StatusBadRequest,
			"service has no gitRepoUrl: register one in service_registry (gitRepoUrl 只能由注册中心登记，"+
				"本机不能设置): "+serviceID)
		return
	}
	// Default: service defaultBranch (usually main) tip — latest code.
	ref := strings.TrimSpace(body.Ref)
	if ref == "" {
		ref = defaultBranchOrMain(svc.DefaultBranch)
	}
	requestID := strings.TrimSpace(body.RequestID)
	if requestID == "" {
		requestID = "pipeline-" + uuid.NewString()[:8]
	}
	if existing, _ := s.store.GetPipeline(requestID); existing != nil {
		writeError(w, http.StatusConflict, "request already exists: "+requestID)
		return
	}
	job, err := s.store.CreatePipeline(requestID, serviceID, ref, by,
		"accepted; package "+ref+" (latest) then deploy with graceful notify+poll")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	byLabel := by.String()
	if byLabel == "" {
		byLabel = "未知"
	}
	_ = s.store.AddPipelineEvent(requestID, eventlevel.Info,
		"流水线已入队：service="+serviceID+" ref="+ref+" 触发者="+byLabel)
	fmt.Printf("[pipeline] %s service=%s ref=%s by=%s\n", requestID, serviceID, ref, by.String())
	if s.pipeline != nil {
		s.pipeline.Kick()
	}
	type resp struct {
		PipelineJob
		Poll string `json:"poll"`
	}
	writeJSON(w, http.StatusAccepted, resp{
		PipelineJob: job,
		Poll:        "/api/pipelines/" + requestID,
	})
}

func (s *apiServer) handleListPipelines(w http.ResponseWriter, r *http.Request) {
	f := parseListFilter(r)
	jobs, total, err := s.store.ListPipelinesFiltered(f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pipelines": jobs,
		"total":     total,
		"page":      f.Page,
		"pageSize":  f.PageSize,
	})
}

// parseListFilter reads the shared history-list query params (filters +
// pagination) for GET /api/deploys and GET /api/pipelines.
func parseListFilter(r *http.Request) ListFilter {
	q := r.URL.Query()
	f := ListFilter{
		ServiceID:       strings.TrimSpace(q.Get("serviceId")),
		State:           strings.TrimSpace(q.Get("state")),
		TriggeredByRole: strings.TrimSpace(q.Get("triggeredByRole")),
		TriggeredByID:   strings.TrimSpace(q.Get("triggeredById")),
		Deployment:      strings.TrimSpace(q.Get("deployment")),
		Version:         strings.TrimSpace(q.Get("version")),
		Ref:             strings.TrimSpace(q.Get("ref")),
		Keyword:         strings.TrimSpace(q.Get("q")),
		From:            strings.TrimSpace(q.Get("from")),
		To:              strings.TrimSpace(q.Get("to")),
	}
	if n, err := strconv.Atoi(q.Get("page")); err == nil {
		f.Page = n
	}
	// pageSize wins; "limit" is kept as an alias for older callers.
	if n, err := strconv.Atoi(q.Get("pageSize")); err == nil {
		f.PageSize = n
	} else if n, err := strconv.Atoi(q.Get("limit")); err == nil {
		f.PageSize = n
	}
	f.normalize()
	return f
}

func (s *apiServer) handleGetPipeline(w http.ResponseWriter, r *http.Request) {
	job, err := s.store.GetPipeline(r.PathValue("requestId"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if job == nil {
		writeError(w, http.StatusNotFound, "pipeline not found")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *apiServer) handleListPipelineEvents(w http.ResponseWriter, r *http.Request) {
	requestID := r.PathValue("requestId")
	job, err := s.store.GetPipeline(requestID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if job == nil {
		writeError(w, http.StatusNotFound, "pipeline not found")
		return
	}
	events, err := s.store.ListPipelineEvents(requestID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

// handleListArtifacts returns artifact metadata, optionally filtered by
// ?serviceId=. The bytes live on GitHub Releases; this is the local index.
func (s *apiServer) handleListArtifacts(w http.ResponseWriter, r *http.Request) {
	serviceID := strings.TrimSpace(r.URL.Query().Get("serviceId"))
	arts, err := s.store.ListArtifacts(serviceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifacts": arts})
}

// handleGetArtifact returns a single artifact by tag (requires ?serviceId=
// because tags are unique per service, not globally).
func (s *apiServer) handleGetArtifact(w http.ResponseWriter, r *http.Request) {
	tag := r.PathValue("tag")
	serviceID := strings.TrimSpace(r.URL.Query().Get("serviceId"))
	if serviceID == "" {
		writeError(w, http.StatusBadRequest, "serviceId query param is required")
		return
	}
	art, err := s.store.GetArtifact(serviceID, tag)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if art == nil {
		writeError(w, http.StatusNotFound, "artifact not found")
		return
	}
	writeJSON(w, http.StatusOK, art)
}

// handleScanArtifacts scans the service repo's GitHub Releases and upserts
// artifact rows, backfilling the local table from existing storage. Requires
// ?serviceId= whose contract has a gitRepoUrl.
func (s *apiServer) handleScanArtifacts(w http.ResponseWriter, r *http.Request) {
	serviceID := strings.TrimSpace(r.URL.Query().Get("serviceId"))
	if serviceID == "" {
		writeError(w, http.StatusBadRequest, "serviceId query param is required")
		return
	}
	svc, err := s.store.GetService(serviceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if svc == nil {
		writeError(w, http.StatusNotFound, "service not found")
		return
	}
	gitURL := resolveServiceGitRepo(r.Context(), s.registry, svc)
	if gitURL == "" {
		writeError(w, http.StatusBadRequest,
			"service has no gitRepoUrl: register one in service_registry（gitRepoUrl 只能由注册中心登记，本机不能设置）")
		return
	}
	if s.storage == nil {
		writeError(w, http.StatusServiceUnavailable, "artifact storage not configured")
		return
	}
	items, err := s.storage.List(r.Context(), gitURL)
	if err != nil {
		writeError(w, http.StatusBadGateway, "scan artifacts: "+err.Error())
		return
	}
	owner, repo, _ := parseRepoOwnerName(gitURL)
	repoSlug := owner + "/" + repo
	storageName := s.storage.Name()
	recorded := 0
	for _, it := range items {
		version := strings.TrimPrefix(it.Tag, "deployment-")
		if err := s.store.RecordArtifact(Artifact{
			ServiceID:          serviceID,
			Tag:                it.Tag,
			Version:            version,
			GitRepoURL:         gitURL,
			RepoSlug:           repoSlug,
			AssetName:          releaseAssetName,
			AssetID:            it.AssetID,
			AssetURL:           it.AssetURL,
			BrowserDownloadURL: it.BrowserDownloadURL,
			ReleaseURL:         it.ReleaseURL,
			Size:               it.Size,
			Storage:            storageName,
			CreatedAt:           nowISO(),
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "record artifact "+it.Tag+": "+err.Error())
			return
		}
		recorded++
	}
	writeJSON(w, http.StatusOK, map[string]any{"scanned": len(items), "recorded": recorded, "serviceId": serviceID, "storage": storageName})
}

func (s *apiServer) handleRestartNotify(w http.ResponseWriter, r *http.Request) {
	var body restartNotifyBody
	_ = json.NewDecoder(r.Body).Decode(&body)
	if strings.TrimSpace(body.RequestID) == "" {
		writeError(w, http.StatusBadRequest, "requestId is required")
		return
	}
	s.drain.Notify(body.RequestID)
	fmt.Printf("[graceful] drain started for requestId=%s\n", body.RequestID)
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "draining": true})
}

func (s *apiServer) handleRestartPoll(w http.ResponseWriter, r *http.Request) {
	if !s.drain.IsDraining() {
		writeJSON(w, http.StatusOK, map[string]any{
			"canRestart": false, "canDeploy": false, "ready": false,
			"draining":    false,
			"reason":      "not notified",
		})
		return
	}
	restartID := s.drain.RestartingID()

	// in-flight deploys, excluding the restarting job itself
	runningDeploys, _ := s.store.ListDeploysByState(StateRunning)
	otherDeploys := 0
	for _, d := range runningDeploys {
		if d.RequestID != restartID {
			otherDeploys++
		}
	}
	// in-flight pipelines (packaging + deploying), excluding the restarting one.
	// A pipeline that is "deploying" but whose deploy job has not started yet
	// (still queued) is NOT in-flight work: the deploy worker deliberately does
	// not claim jobs while draining, so counting it as in-flight deadlocks the
	// restart — the poll never reports ready and the queued deploy never starts
	// (both wait on each other until the max-wait timeout forces a restart).
	// Such a deploy is only a row in the store; the process that comes up after
	// the restart claims and runs it.
	packaging, _ := s.store.ListPipelinesByState(PipelinePackaging)
	deploying, _ := s.store.ListPipelinesByState(PipelineDeploying)
	otherPipelines := 0
	for _, p := range append(packaging, deploying...) {
		if p.RequestID == restartID || p.DeployRequestID == restartID {
			continue
		}
		if p.State == PipelineDeploying && !s.deployRunning(p.DeployRequestID) {
			continue
		}
		otherPipelines++
	}

	canRestart := otherDeploys == 0 && otherPipelines == 0
	writeJSON(w, http.StatusOK, map[string]any{
		"canRestart":     canRestart,
		"canDeploy":      canRestart,
		"ready":           canRestart,
		"draining":        true,
		"restartingId":    restartID,
		"inflightDeploys": otherDeploys,
		"inflightPipes":   otherPipelines,
	})
}

// deployRunning reports whether the deploy job backing a "deploying" pipeline
// has actually started (state running). A queued job has not touched the
// runtime yet, so it is not in-flight work and must not hold back a graceful
// self-restart; unknown/finished jobs are likewise not in-flight.
func (s *apiServer) deployRunning(requestID string) bool {
	if strings.TrimSpace(requestID) == "" {
		return false
	}
	dep, err := s.store.GetDeploy(requestID)
	if err != nil || dep == nil {
		return false
	}
	return dep.State == StateRunning
}

func (s *apiServer) handleMeta(w http.ResponseWriter, r *http.Request) {
	example, _ := normalizeDeploymentTag("abc12345")
	writeJSON(w, http.StatusOK, map[string]any{
		"home":                    s.cfg.Home,
		"packagesDir":             s.cfg.PackagesDir,
		"port":                    s.cfg.Port,
		"normalizeExample":        example,
		"identityEnforce":         s.cfg.IdentityEnforce,
		"identityHeaders":         []string{identityRoleHeader, identityIDHeader},
		"gracefulPollIntervalSec": int(gracefulPollInterval / time.Second),
		"gracefulMaxWaitMs":       int(s.cfg.GracefulMaxWait / time.Millisecond),
		"releaseMaxSec":           s.cfg.ReleaseMaxSec,
		"artifactStorage":         s.storageName(),
		"githubReleaseEnabled":    s.cfg.GitHubToken != "",
		"deployNotify":            "POST /api/deploy-notify {serviceId, ref?}",
		"serviceRegistryUrl":      s.registry.BaseURL(),
		"serviceRegistryEnabled":  s.registry.Enabled(),
		"serviceCatalog":          "GET /api/services（服务列表来自 service_registry，本机只存部署配置）",
	})
}

// storageName reports the active artifact storage backend for /api/meta, or
// "" when storage is not initialized (e.g. during early bootstrap).
func (s *apiServer) storageName() string {
	if s.storage == nil {
		return ""
	}
	return s.storage.Name()
}
