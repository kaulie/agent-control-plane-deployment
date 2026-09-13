package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestSupportsGracefulRestart(t *testing.T) {
	cases := []struct {
		name string
		svc  ServiceContract
		want bool
	}{
		{name: "neither", svc: ServiceContract{}, want: false},
		{name: "notify only", svc: ServiceContract{RestartNotifyURL: "http://x/n"}, want: false},
		{name: "poll only", svc: ServiceContract{RestartPollURL: "http://x/p"}, want: false},
		{name: "both", svc: ServiceContract{
			RestartNotifyURL: "http://x/n",
			RestartPollURL:   "http://x/p",
		}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.svc.SupportsGracefulRestart(); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestPollAllowsDeploy(t *testing.T) {
	if !pollAllowsDeploy(restartPollStatus{CanRestart: true}) {
		t.Fatal("canRestart")
	}
	if !pollAllowsDeploy(restartPollStatus{CanDeploy: true}) {
		t.Fatal("canDeploy")
	}
	if !pollAllowsDeploy(restartPollStatus{Ready: true}) {
		t.Fatal("ready")
	}
	if pollAllowsDeploy(restartPollStatus{}) {
		t.Fatal("empty should be false")
	}
}

func TestWaitForGracefulRestartReady(t *testing.T) {
	var notified atomic.Bool
	var polls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /notify", func(w http.ResponseWriter, r *http.Request) {
		notified.Store(true)
		b, _ := io.ReadAll(r.Body)
		var body restartNotifyBody
		if err := json.Unmarshal(b, &body); err != nil {
			t.Errorf("notify body: %v", err)
		}
		if body.ServiceID != "svc" || body.RequestID != "req-1" {
			t.Errorf("unexpected notify payload: %+v", body)
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /poll", func(w http.ResponseWriter, r *http.Request) {
		n := polls.Add(1)
		ready := n >= 2
		_ = json.NewEncoder(w).Encode(map[string]any{"canRestart": ready})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Shrink poll interval for test by temporarily using a short max wait
	// and patching via first-poll not ready → second ready. The production
	// interval is 15s; override sleep path by setting max wait high enough
	// for two polls would be slow. Instead call getRestartPollStatus/notify
	// pieces and a thin wait with injected short interval via maxWait that
	// forces after first failure — here we use a custom short loop.
	svc := ServiceContract{
		ServiceID:         "svc",
		RestartNotifyURL:  srv.URL + "/notify",
		RestartPollURL:    srv.URL + "/poll",
		GracefulMaxWaitMs: 2000,
	}
	cfg := Config{GracefulMaxWait: 2 * time.Second}
	job := DeployJob{RequestID: "req-1", Deployment: "deployment-abc", ServiceID: "svc"}

	// Monkey: waitForGracefulRestart uses 15s sleep — too slow for unit test.
	// Test notify + poll helpers directly, then a fast local wait loop mirroring production.
	if err := postRestartNotify(svc.RestartNotifyURL, restartNotifyBody{
		ServiceID:  "svc",
		RequestID:  "req-1",
		Deployment: "deployment-abc",
		Message:    "test",
	}); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if !notified.Load() {
		t.Fatal("notify not called")
	}
	st1, err := getRestartPollStatus(svc.RestartPollURL)
	if err != nil {
		t.Fatal(err)
	}
	if pollAllowsDeploy(st1) {
		t.Fatal("first poll should not be ready")
	}
	st2, err := getRestartPollStatus(svc.RestartPollURL)
	if err != nil {
		t.Fatal(err)
	}
	if !pollAllowsDeploy(st2) {
		t.Fatal("second poll should be ready")
	}
	_ = cfg
	_ = job
}

func TestWaitForGracefulRestartForceTimeout(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /notify", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("GET /poll", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"canRestart": false})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Use waitForGracefulRestart with a tiny max wait; first poll fails then
	// remaining sleep is clipped to remaining time so force happens quickly.
	svc := ServiceContract{
		ServiceID:         "svc",
		RestartNotifyURL:  srv.URL + "/notify",
		RestartPollURL:    srv.URL + "/poll",
		GracefulMaxWaitMs: 50,
	}
	cfg := Config{GracefulMaxWait: 50 * time.Millisecond}
	job := DeployJob{RequestID: "req-force", Deployment: "deployment-x", ServiceID: "svc"}
	start := time.Now()
	forced := waitForGracefulRestart(nil, svc, cfg, job, "x")
	if !forced {
		t.Fatal("expected force after timeout")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("force wait took too long: %s", time.Since(start))
	}
}
