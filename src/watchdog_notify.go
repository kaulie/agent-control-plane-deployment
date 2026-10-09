package main

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"
)

// notifyWatchdog asks agent-watchdog to reconcile its probe list now.
// Failures are logged only — saving a contract must not depend on :4230.
func notifyWatchdog(base string) {
	base = strings.TrimSpace(base)
	if base == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		url := strings.TrimRight(base, "/") + "/api/sync"
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
		if err != nil {
			log.Printf("[watchdog] notify build: %v", err)
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			log.Printf("[watchdog] notify %s: %v", url, err)
			return
		}
		_ = resp.Body.Close()
		if resp.StatusCode >= 300 {
			log.Printf("[watchdog] notify %s: HTTP %d", url, resp.StatusCode)
		}
	}()
}
