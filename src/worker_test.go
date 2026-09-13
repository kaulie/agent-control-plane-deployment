package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestWaitForHealthAlreadyHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if !waitForHealth(srv.URL, 2*time.Second) {
		t.Fatal("expected true for already-healthy endpoint")
	}
}

func TestWaitForHealthBecomesHealthy(t *testing.T) {
	var healthy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if healthy.Load() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	// Flip to healthy shortly after start, well within the wait window.
	go func() {
		time.Sleep(300 * time.Millisecond)
		healthy.Store(true)
	}()
	if !waitForHealth(srv.URL, 5*time.Second) {
		t.Fatal("expected true once the endpoint became healthy within timeout")
	}
}

func TestWaitForHealthNeverHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	start := time.Now()
	if waitForHealth(srv.URL, 1*time.Second) {
		t.Fatal("expected false when endpoint never becomes healthy")
	}
	// Should have waited roughly the full timeout (not bailed instantly).
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("waitForHealth returned too early: %v", elapsed)
	}
}
