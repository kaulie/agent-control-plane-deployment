package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func seedDefaultService(store *Store) {
	svc, err := store.GetService("web-cursor")
	if err != nil || svc != nil {
		return
	}
	home, _ := os.UserHomeDir()
	runtimeDir := filepath.Join(home, "runtime", "web-cursor")
	_, err = store.UpsertService(ServiceContract{
		ServiceID:  "web-cursor",
		Name:       "Web Cursor Agent Gateway",
		RuntimeDir: runtimeDir,
		HealthURL:  "http://127.0.0.1:4211/health",
		StartCmd:   fmt.Sprintf("bash %q", filepath.Join(runtimeDir, "scripts", "start.sh")),
		StopCmd:    fmt.Sprintf("bash %q", filepath.Join(runtimeDir, "scripts", "stop.sh")),
		RestartCmd: fmt.Sprintf("bash %q", filepath.Join(runtimeDir, "scripts", "restart.sh")),
		GitRepoURL: "https://github.com/kaulie/agent-control-plane",
	})
	if err != nil {
		log.Printf("[seed] failed: %v", err)
		return
	}
	log.Printf("[seed] registered service web-cursor → %s", runtimeDir)
}

func seedACPService(store *Store, cfg Config) {
	svc, err := store.GetService("agent-control-plane-deployment")
	if err != nil || svc != nil {
		return
	}
	_, err = store.UpsertService(ServiceContract{
		ServiceID:         "agent-control-plane-deployment",
		Name:               "Agent Control Plane Deployment",
		RuntimeDir:         cfg.Home,
		HealthURL:          fmt.Sprintf("http://127.0.0.1:%d/health", cfg.Port),
		StartCmd:           fmt.Sprintf("bash %q", filepath.Join(cfg.Home, "scripts", "start.sh")),
		StopCmd:            fmt.Sprintf("bash %q", filepath.Join(cfg.Home, "scripts", "stop.sh")),
		RestartCmd:         fmt.Sprintf("bash %q", filepath.Join(cfg.Home, "scripts", "restart.sh")),
		RestartNotifyURL:   fmt.Sprintf("http://127.0.0.1:%d/restart/notify", cfg.Port),
		RestartPollURL:     fmt.Sprintf("http://127.0.0.1:%d/restart/poll", cfg.Port),
	})
	if err != nil {
		log.Printf("[seed] acp service failed: %v", err)
		return
	}
	log.Printf("[seed] registered service agent-control-plane-deployment → %s", cfg.Home)
}

func main() {
	cfg := loadConfig()
	store, err := NewStore(cfg.DBPath)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer store.Close()

	storage, err := NewArtifactStorage(cfg)
	if err != nil {
		log.Fatalf("artifact storage: %v", err)
	}
	log.Printf("[storage] artifact storage backend: %s", storage.Name())

	seedDefaultService(store)
	seedACPService(store, cfg)

	registry := NewServiceRegistry(cfg)
	if registry.Enabled() {
		log.Printf("[registry] service catalog pulled from %s", registry.BaseURL())
	} else {
		log.Printf("[registry] SERVICE_REGISTRY_URL=off: only locally configured services are shown")
	}

	drain := &GracefulDrain{}
	worker := NewDeployWorker(store, cfg, storage, drain)
	pipeline := NewPipelineWorker(store, cfg, storage, worker, drain)
	pipeline.registry = registry
	api := &apiServer{store: store, cfg: cfg, storage: storage, worker: worker, pipeline: pipeline, drain: drain, registry: registry}

	pidFile := filepath.Join(cfg.Home, "deployment.pid")
	_ = os.WriteFile(pidFile, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644)

	httpServer := &http.Server{
		Addr:    net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port)),
		Handler: api.routes(),
	}

	// Start serving BEFORE reconcile so that self-deploy orphan reconciliation
	// can confirm this server's own /health. Previously reconcile ran before
	// ListenAndServe, so a just-succeeded self-upgrade was falsely marked
	// "failed" (healthOK hit /health before the listener was up).
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()
	healthURL := fmt.Sprintf("http://%s/health", httpServer.Addr)
	readyDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(readyDeadline) {
		if healthOK(healthURL) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	cleared := clearStaleDeployPauses(store)
	if cleared > 0 {
		log.Printf("cleared stale deploy pause flags for %d service(s)", cleared)
	}
	reconciled := reconcileOrphanDeploys(store)
	if reconciled > 0 {
		log.Printf("reconciled %d orphan deploy(s) left running", reconciled)
	}
	worker.Start()
	pipeline.Start()

	stopCh := make(chan struct{})
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
		pipeline.Stop()
		worker.Stop()
		_ = os.Remove(pidFile)
		_ = httpServer.Close()
		close(stopCh)
	}()

	log.Printf("deployment service on http://%s home=%s", httpServer.Addr, cfg.Home)
	<-stopCh
}
