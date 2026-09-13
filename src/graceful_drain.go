package main

import (
	"sync"
	"sync/atomic"
)

// GracefulDrain coordinates graceful self-restart of the deployment control
// plane. When /restart/notify is received, the server enters draining: the
// deploy/pipeline workers stop claiming new jobs, and /restart/poll reports
// canRestart=true once no other in-flight work remains (the restarting job
// itself is excluded). The acp-upgrader then stops/starts the process.
type GracefulDrain struct {
	draining atomic.Bool
	mu       sync.Mutex
	restartID string // requestID of the deploy/pipeline being restarted, excluded from "in-flight"
}

// Notify marks the server as draining for the given restart requestID.
func (g *GracefulDrain) Notify(requestID string) {
	if g == nil {
		return
	}
	g.draining.Store(true)
	g.mu.Lock()
	g.restartID = requestID
	g.mu.Unlock()
}

// IsDraining reports whether the server is currently draining.
func (g *GracefulDrain) IsDraining() bool {
	if g == nil {
		return false
	}
	return g.draining.Load()
}

// RestartingID returns the requestID that triggered the drain (excluded from
// the in-flight check), or "" if not draining.
func (g *GracefulDrain) RestartingID() string {
	if g == nil {
		return ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.restartID
}

// Clear exits draining mode. Called when a self-deploy fails before the
// upgrader handoff (so workers can resume). On success the process is killed,
// so clearing is unnecessary and intentionally skipped by callers.
func (g *GracefulDrain) Clear() {
	if g == nil {
		return
	}
	g.draining.Store(false)
	g.mu.Lock()
	g.restartID = ""
	g.mu.Unlock()
}
