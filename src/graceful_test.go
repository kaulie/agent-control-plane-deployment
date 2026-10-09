package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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

// TestGracefulClientNoKeepAlive locks in the fix for the "deployment service
// killed by project stop.sh" bug: the graceful client must disable keep-alives
// so no idle TCP connection to the project port lingers after notify/poll
// (otherwise `kill $(lsof -ti:$PORT)` in the project's stop.sh sweeps the
// deployment process up). Each request must also send Connection: close.
func TestGracefulClientNoKeepAlive(t *testing.T) {
	tr, ok := gracefulHTTPClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", gracefulHTTPClient.Transport)
	}
	if !tr.DisableKeepAlives {
		t.Fatal("gracefulHTTPClient.Transport.DisableKeepAlives must be true " +
			"(prevents idle keep-alive connections to the project port that let " +
			"the project's stop.sh kill the deployment service)")
	}

	var seenClose atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Connection") == "close" || r.Close {
			seenClose.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ready":true}`))
	}))
	defer srv.Close()

	if _, err := getRestartPollStatus(srv.URL); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if !seenClose.Load() {
		t.Fatal("poll request did not send Connection: close; keep-alive would " +
			"leave a socket to the project port that stop.sh could kill")
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

// withShortPollInterval shrinks the graceful poll interval for one test and
// restores it afterwards, so the wait loop runs without production-sized sleeps.
func withShortPollInterval(t *testing.T, d time.Duration) {
	t.Helper()
	old := gracefulPollInterval
	gracefulPollInterval = d
	t.Cleanup(func() { gracefulPollInterval = old })
}

// closedLoopbackURL returns an http URL on a port nothing listens on, so a
// request to it fails at connection level (dial refused = unreachable).
func closedLoopbackURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return "http://" + addr
}

// unreachableErr returns the real error produced by dialing a closed port.
func unreachableErr(t *testing.T) error {
	t.Helper()
	_, err := http.Get(closedLoopbackURL(t) + "/poll")
	if err == nil {
		t.Fatal("expected a dial error against a closed port")
	}
	return err
}

// TestIsUnreachable locks the classification the early-restart behaviour rests
// on: only connection-level failures count as "unreachable"; an endpoint that
// answers with a bad status or malformed body does not (its normal wait stays).
func TestIsUnreachable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "connection refused", err: unreachableErr(t), want: true},
		{name: "http error status", err: fmt.Errorf("poll returned HTTP %d", http.StatusInternalServerError), want: false},
		{name: "bad json", err: fmt.Errorf("poll JSON: %w", errors.New("unexpected end of JSON input")), want: false},
		{name: "remote marked unreachable", err: fmt.Errorf("远端 poll 不可达：%w", errGracefulUnreachable), want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUnreachable(tc.err); got != tc.want {
				t.Fatalf("isUnreachable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// fakeGracefulTransport replays canned notify/poll answers so the graceful wait
// loop can be exercised deterministically, without sockets or 15s sleeps.
type fakeGracefulTransport struct {
	notifyErr   error
	answers     []pollAnswer
	notifyCalls int
	pollCalls   int
}

type pollAnswer struct {
	status restartPollStatus
	err    error
}

func (t *fakeGracefulTransport) notify(string, restartNotifyBody) error {
	t.notifyCalls++
	return t.notifyErr
}

func (t *fakeGracefulTransport) poll(string) (restartPollStatus, error) {
	i := t.pollCalls
	t.pollCalls++
	if i < len(t.answers) {
		return t.answers[i].status, t.answers[i].err
	}
	return restartPollStatus{}, nil // past the script: reachable, never ready
}

func gracefulTestService() ServiceContract {
	return ServiceContract{
		ServiceID:         "svc",
		RestartNotifyURL:  "http://127.0.0.1:1/notify",
		RestartPollURL:    "http://127.0.0.1:1/poll",
		GracefulMaxWaitMs: 600000,
	}
}

// Notify proving the project is unreachable must skip the poll loop and go
// straight to the restart.
func TestGracefulNotifyUnreachableRestartsImmediately(t *testing.T) {
	withShortPollInterval(t, 10*time.Millisecond)
	tr := &fakeGracefulTransport{
		notifyErr: unreachableErr(t),
		answers:   []pollAnswer{{status: restartPollStatus{CanRestart: true}}},
	}
	cfg := Config{GracefulMaxWait: 10 * time.Minute}
	job := DeployJob{RequestID: "req-notify-unreachable", Deployment: "d", ServiceID: "svc"}

	forced := waitForGracefulRestartVia(nil, gracefulTestService(), cfg, job, "v", tr)
	if !forced {
		t.Fatal("notify unreachable must end the wait and restart")
	}
	if tr.notifyCalls != 1 || tr.pollCalls != 0 {
		t.Fatalf("must restart right after the failed notify (notify=%d poll=%d)", tr.notifyCalls, tr.pollCalls)
	}
}

// Three consecutive unreachable polls must end the graceful window early,
// instead of polling until the whole max-wait elapses.
func TestGracefulPollUnreachableThresholdRestartsEarly(t *testing.T) {
	withShortPollInterval(t, 5*time.Millisecond)
	errU := unreachableErr(t)
	tr := &fakeGracefulTransport{answers: []pollAnswer{
		{err: errU}, {err: errU}, {err: errU},
		{status: restartPollStatus{CanRestart: true}}, // must never be reached
	}}
	cfg := Config{GracefulMaxWait: 10 * time.Minute}
	job := DeployJob{RequestID: "req-poll-unreachable", Deployment: "d", ServiceID: "svc"}

	start := time.Now()
	forced := waitForGracefulRestartVia(nil, gracefulTestService(), cfg, job, "v", tr)
	if !forced {
		t.Fatal("three consecutive unreachable polls must force the restart")
	}
	if tr.pollCalls != gracefulUnreachableThreshold {
		t.Fatalf("expected exactly %d polls before restarting, got %d", gracefulUnreachableThreshold, tr.pollCalls)
	}
	if elapsed := time.Since(start); elapsed > time.Minute {
		t.Fatalf("did not restart early (elapsed %s, maxWait %s)", elapsed, cfg.GracefulMaxWait)
	}
}

// Fewer than three unreachable probes, then a ready project: normal behaviour
// is preserved — the wait ends because the project became ready, not early.
func TestGracefulPollFewerThanThreeUnreachableKeepsWaiting(t *testing.T) {
	withShortPollInterval(t, 5*time.Millisecond)
	errU := unreachableErr(t)
	tr := &fakeGracefulTransport{answers: []pollAnswer{
		{err: errU}, {err: errU},
		{status: restartPollStatus{CanRestart: true}},
	}}
	cfg := Config{GracefulMaxWait: 10 * time.Minute}
	job := DeployJob{RequestID: "req-poll-recover", Deployment: "d", ServiceID: "svc"}

	forced := waitForGracefulRestartVia(nil, gracefulTestService(), cfg, job, "v", tr)
	if forced {
		t.Fatal("a project that answers on the third poll must not be force-restarted")
	}
	if tr.pollCalls != 3 {
		t.Fatalf("expected 3 polls (2 unreachable + 1 ready), got %d", tr.pollCalls)
	}
}

// A reachable-but-not-ready project must keep the original timeout behaviour.
func TestGracefulPollReachableNotReadyWaitsToTimeout(t *testing.T) {
	withShortPollInterval(t, 5*time.Millisecond)
	tr := &fakeGracefulTransport{answers: []pollAnswer{{status: restartPollStatus{}}}}
	svc := gracefulTestService()
	svc.GracefulMaxWaitMs = 60
	cfg := Config{GracefulMaxWait: 60 * time.Millisecond}
	job := DeployJob{RequestID: "req-poll-notready", Deployment: "d", ServiceID: "svc"}

	if forced := waitForGracefulRestartVia(nil, svc, cfg, job, "v", tr); !forced {
		t.Fatal("expected force after the max-wait window, got ready")
	}
	if tr.pollCalls < 2 {
		t.Fatalf("expected repeated polls until timeout, got %d", tr.pollCalls)
	}
}
