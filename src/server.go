package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

type apiServer struct {
	store  *Store
	cfg    Config
	worker *DeployWorker
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
	mux.HandleFunc("GET /api/meta", s.handleMeta)
	return withCORS(mux)
}

func (s *apiServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"service": "agent-control-plane-deployment",
		"home":    s.cfg.Home,
		"time":    nowISO(),
	})
}

func (s *apiServer) handleListServices(w http.ResponseWriter, r *http.Request) {
	svcs, err := s.store.ListServices()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"services": svcs})
}

func (s *apiServer) handleGetService(w http.ResponseWriter, r *http.Request) {
	svc, err := s.store.GetService(r.PathValue("serviceId"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if svc == nil {
		writeError(w, http.StatusNotFound, "service not found")
		return
	}
	writeJSON(w, http.StatusOK, svc)
}

type putServiceBody struct {
	Name              string  `json:"name"`
	RuntimeDir        string  `json:"runtimeDir"`
	HealthURL         string  `json:"healthUrl"`
	StartCmd          string  `json:"startCmd"`
	StopCmd           string  `json:"stopCmd"`
	RestartCmd        string  `json:"restartCmd"`
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

	name := strings.TrimSpace(body.Name)
	runtimeDir := strings.TrimSpace(body.RuntimeDir)
	healthURL := strings.TrimSpace(body.HealthURL)
	startCmd := strings.TrimSpace(body.StartCmd)
	stopCmd := strings.TrimSpace(body.StopCmd)
	restartCmd := strings.TrimSpace(body.RestartCmd)
	notifyURL := ""
	pollURL := ""
	maxWaitMs := 0
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
		StartCmd:          startCmd,
		StopCmd:           stopCmd,
		RestartCmd:        restartCmd,
		RestartNotifyURL:  notifyURL,
		RestartPollURL:    pollURL,
		GracefulMaxWaitMs: maxWaitMs,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	status := http.StatusCreated
	if existing != nil {
		status = http.StatusOK
	}
	writeJSON(w, status, svc)
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
	deployment, err := assertPackage(s.cfg.PackagesDir, raw)
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
	job, err := s.store.CreateDeploy(requestID, serviceID, deployment, "queued for deployment worker")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.worker.Kick()

	type resp struct {
		DeployJob
		Poll string `json:"poll"`
	}
	writeJSON(w, http.StatusAccepted, resp{DeployJob: job, Poll: "/api/deploys/" + requestID})
}

func (s *apiServer) handleListDeploys(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil {
			limit = n
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 200 {
		limit = 200
	}
	deploys, err := s.store.ListDeploys(limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deploys": deploys})
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

func (s *apiServer) handleMeta(w http.ResponseWriter, r *http.Request) {
	example, _ := normalizeDeploymentTag("abc12345")
	writeJSON(w, http.StatusOK, map[string]any{
		"home":                    s.cfg.Home,
		"packagesDir":             s.cfg.PackagesDir,
		"port":                    s.cfg.Port,
		"normalizeExample":        example,
		"gracefulPollIntervalSec": int(gracefulPollInterval / time.Second),
		"gracefulMaxWaitMs":       int(s.cfg.GracefulMaxWait / time.Millisecond),
	})
}
