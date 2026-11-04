package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClassifyFailureKnownCases(t *testing.T) {
	cases := []struct {
		err      string
		message  string
		proxy    bool
		stage    string
		category string
	}{
		{"unknown service: web", "pipeline failed", false, "precheck", "unknown_service"},
		{"service has no gitRepoUrl: register one", "pipeline failed", false, "precheck", "no_repo"},
		{"package failed: git [fetch --depth 1 origin main]: dial tcp i/o timeout", "pipeline failed", false, "packaging", "proxy_off"},
		{"package failed: git fetch: proxy connection refused", "pipeline failed", true, "packaging", "proxy_on"},
		{"package failed: build.sh: exit status 1", "pipeline failed", false, "packaging", "build"},
		{"enqueue deploy failed: duplicate key", "pipeline failed", false, "enqueue", "enqueue"},
		{"deploy machine gpu-2 has no deploy channel (DEPLOY_MACHINE_TARGETS)", "deploy failed", false, "deploy", "no_channel"},
		{"restart finished but health check failed: http://127.0.0.1:9527/health", "deploy failed", false, "deploy", "health_check"},
		{"artifacts staged but acp-upgrader is not running; start scripts/upgrader-start.sh", "deploy failed", false, "deploy", "upgrader"},
		{"something unexpected", "deploy failed", false, "deploy", "uncategorized"},
	}
	for _, tc := range cases {
		got := classifyFailure(tc.err, tc.message, tc.proxy)
		if got.Stage != tc.stage || got.Category != tc.category {
			t.Errorf("%q: got %s/%s, want %s/%s", tc.err, got.Stage, got.Category, tc.stage, tc.category)
		}
		if got.Summary == "" || len(got.Suggestions) == 0 {
			t.Errorf("%q: summary or suggestions empty", tc.err)
		}
	}
}

func TestDiagnoseStartFailureBanner(t *testing.T) {
	d := diagnoseFrom(diagInput{
		Job: PipelineJob{
			RequestID: "pipeline-banner",
			State:     PipelineFailed,
			Error:     "start failed: [start][错误] 30s 内 /health 未就绪，最近日志：\nINFO noise",
			Message:   "deploy failed",
		},
	})
	if d.Category != "start_failed" {
		t.Fatalf("category = %s", d.Category)
	}
	if !strings.Contains(d.Cause, "30s 内 /health 未就绪") {
		t.Fatalf("cause = %s", d.Cause)
	}
}

func TestDiagnoseClippedStartKilledBeforeListen(t *testing.T) {
	start := time.Date(2026, 11, 4, 7, 33, 2, 0, time.UTC)
	fail := start.Add(44 * time.Second)
	workerAt := start.Add(12 * time.Second).In(time.Local).Format("2006-01-02 15:04:05")

	root := t.TempDir()
	logDir := filepath.Join(root, "logs")
	if err := os.MkdirAll(filepath.Join(root, "backend"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	env := "BRAIN_LOG_DIR=" + logDir + "\nDEEPSEEK_API_KEY=secret-should-not-appear\n"
	if err := os.WriteFile(filepath.Join(root, "backend", ".env"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	brain := workerAt + " | INFO | thread:1 | llm worker started, scan task begin..\n" +
		start.Add(-time.Minute).In(time.Local).Format("2006-01-02 15:04:05") + " | INFO | GET /health HTTP/1.1\" 200 -\n"
	if err := os.WriteFile(filepath.Join(logDir, "brain.log"), []byte(brain), 0o644); err != nil {
		t.Fatal(err)
	}

	tail := strings.Repeat("2026-11-04 15:32:48 | INFO | thread:1 | GET /health HTTP/1.1\" 200 -\n", 40)
	d := diagnoseFrom(diagInput{
		Job: PipelineJob{
			RequestID:  "pipeline-8c58ee70",
			State:      PipelineFailed,
			Error:      "start failed: " + tail,
			Message:    "deploy failed",
			FinishedAt: fail.Format(time.RFC3339Nano),
		},
		Events: []diagEvent{
			{Source: "deploy", Level: "info", Ts: start.Format(time.RFC3339Nano), Message: "执行 startCmd（SERVICE_PORT=9527）:bash scripts/start.sh"},
			{Source: "deploy", Level: "error", Ts: fail.Format(time.RFC3339Nano), Message: "start 失败：" + tail},
		},
		RuntimeDir: root,
		ReadLogs:   true,
	})
	if d.Category != "start_failed" {
		t.Fatalf("category = %s", d.Category)
	}
	if !strings.Contains(d.Cause, "/health") || !strings.Contains(d.Cause, "监听") {
		t.Fatalf("cause = %s", d.Cause)
	}
	blob := d.Cause + "\n" + strings.Join(evidenceText(d), "\n")
	if strings.Contains(blob, "secret-should-not-appear") {
		t.Fatal("diagnosis leaked an env value")
	}
	if strings.Contains(d.Cause, "| INFO |") {
		t.Fatal("cause should not be the info log tail")
	}
}

func TestClipCmdOutKeepsFailureHead(t *testing.T) {
	head := "[start][错误] 30s 内 /health 未就绪，最近日志："
	out := head + "\n" + strings.Repeat("INFO line padding so the output exceeds two thousand bytes.\n", 80)
	got := clipCmdOut(out)
	if len(got) > 2500 {
		t.Fatalf("clipped len = %d", len(got))
	}
	if !strings.Contains(got, "[start][错误]") {
		t.Fatalf("head was dropped: %s", got[:80])
	}
	if !strings.Contains(got, "中间已截断") {
		t.Fatal("missing truncation marker")
	}
	short := clipCmdOut("start failed: exit 1")
	if short != "start failed: exit 1" {
		t.Fatalf("short output changed: %q", short)
	}
}

func TestDiagnoseHTTP(t *testing.T) {
	s := newTestStore(t)
	api := &apiServer{store: s}
	h := api.routes()

	if rec := diagnoseGet(t, h, "/api/pipelines/missing/diagnosis"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing: %d %s", rec.Code, rec.Body.String())
	}

	if _, err := s.CreatePipeline("pipeline-ok", "svc", "main", false, Identity{}, "queued"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdatePipeline("pipeline-ok", PipelineJob{State: PipelineSucceeded, Message: "pipeline succeeded"}); err != nil {
		t.Fatal(err)
	}
	if rec := diagnoseGet(t, h, "/api/pipelines/pipeline-ok/diagnosis"); rec.Code != http.StatusConflict {
		t.Fatalf("succeeded: %d %s", rec.Code, rec.Body.String())
	}

	if _, err := s.CreatePipeline("pipeline-bad", "svc", "main", true, Identity{}, "queued"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdatePipeline("pipeline-bad", PipelineJob{
		State:   PipelineFailed,
		Error:   "package failed: git fetch: proxy connection refused",
		Message: "pipeline failed",
	}); err != nil {
		t.Fatal(err)
	}
	rec := diagnoseGet(t, h, "/api/pipelines/pipeline-bad/diagnosis")
	if rec.Code != http.StatusOK {
		t.Fatalf("failed pipeline: %d %s", rec.Code, rec.Body.String())
	}
	var d Diagnosis
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Category != "proxy_on" || d.RequestID != "pipeline-bad" {
		t.Fatalf("diagnosis = %+v", d)
	}
}

func diagnoseGet(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func evidenceText(d Diagnosis) []string {
	out := make([]string, 0, len(d.Evidence))
	for _, ev := range d.Evidence {
		out = append(out, ev.Message)
	}
	return out
}
