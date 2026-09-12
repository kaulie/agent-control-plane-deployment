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
	})
	if err != nil {
		log.Printf("[seed] failed: %v", err)
		return
	}
	log.Printf("[seed] registered service web-cursor → %s", runtimeDir)
}

func main() {
	cfg := loadConfig()
	store, err := NewStore(cfg.DBPath)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer store.Close()

	seedDefaultService(store)

	worker := NewDeployWorker(store, cfg)
	api := &apiServer{store: store, cfg: cfg, worker: worker}

	pidFile := filepath.Join(cfg.Home, "deployment.pid")
	_ = os.WriteFile(pidFile, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644)

	httpServer := &http.Server{
		Addr:    net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port)),
		Handler: api.routes(),
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

	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
		worker.Stop()
		_ = os.Remove(pidFile)
		_ = httpServer.Close()
	}()

	log.Printf("deployment service on http://%s home=%s", httpServer.Addr, cfg.Home)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("listen: %v", err)
	}
}
