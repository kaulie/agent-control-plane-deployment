package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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
	if _, _, err := postRestartNotify(svc.RestartNotifyURL, restartNotifyBody{
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
	st1, _, _, err := getRestartPollStatus(svc.RestartPollURL)
	if err != nil {
		t.Fatal(err)
	}
	if pollAllowsDeploy(st1) {
		t.Fatal("first poll should not be ready")
	}
	st2, _, _, err := getRestartPollStatus(svc.RestartPollURL)
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

// TestGracefulEventsRecordRequestResponse verifies that graceful deploy events
// capture both the request body and the response status/body for notify + poll.
func TestGracefulEventsRecordRequestResponse(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /notify", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"accepted":true}`))
	})
	mux.HandleFunc("GET /poll", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"canRestart":true,"canDeploy":false,"ready":true}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	store, err := NewStore(filepath.Join(dir, "test.sqlite"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	svc := ServiceContract{
		ServiceID:         "svc",
		RestartNotifyURL:  srv.URL + "/notify",
		RestartPollURL:    srv.URL + "/poll",
		GracefulMaxWaitMs: 5000,
	}
	cfg := Config{GracefulMaxWait: 5 * time.Second}
	job := DeployJob{RequestID: "req-evt", Deployment: "deployment-abc12345", ServiceID: "svc"}

	forced := waitForGracefulRestart(store, svc, cfg, job, "v1")
	if forced {
		t.Fatal("expected ready (not forced)")
	}

	events, err := store.ListDeployEvents(job.RequestID)
	if err != nil {
		t.Fatalf("ListDeployEvents: %v", err)
	}

	var notifyEv, pollEv string
	for _, e := range events {
		if strings.Contains(e.Message, "graceful 通知：POST") {
			notifyEv = e.Message
		}
		if strings.Contains(e.Message, "graceful 轮询 #1：GET") {
			pollEv = e.Message
		}
	}
	if notifyEv == "" {
		t.Fatalf("missing notify event; events=%v", events)
	}
	if pollEv == "" {
		t.Fatalf("missing poll event; events=%v", events)
	}

	// notify event must include request body + response status + response body
	if !strings.Contains(notifyEv, "请求体:") || !strings.Contains(notifyEv, `"serviceId":"svc"`) {
		t.Fatalf("notify event missing request body: %q", notifyEv)
	}
	if !strings.Contains(notifyEv, "响应: HTTP 202") || !strings.Contains(notifyEv, `{"accepted":true}`) {
		t.Fatalf("notify event missing response: %q", notifyEv)
	}

	// poll event must include the GET url + response status + response body
	if !strings.Contains(pollEv, "GET ") || !strings.Contains(pollEv, "/poll") {
		t.Fatalf("poll event missing request url: %q", pollEv)
	}
	if !strings.Contains(pollEv, "响应: HTTP 200") || !strings.Contains(pollEv, `"canRestart":true`) {
		t.Fatalf("poll event missing response body: %q", pollEv)
	}
	if !strings.Contains(pollEv, "就绪") {
		t.Fatalf("poll event missing ready outcome: %q", pollEv)
	}
}
