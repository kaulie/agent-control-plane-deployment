package deploymcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewClientRejectsBadURL(t *testing.T) {
	if _, err := NewClient("ftp://127.0.0.1:4220", "agent", "deployment-mcp", time.Second); err == nil {
		t.Fatal("ftp URL should be rejected")
	}
	if _, err := NewClient("http://", "agent", "deployment-mcp", time.Second); err == nil {
		t.Fatal("URL without host should be rejected")
	}
}

func TestClientSendsIdentityQueryAndBody(t *testing.T) {
	var got struct {
		method string
		path   string
		role   string
		id     string
		body   map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.path = r.URL.Path
		got.role = r.Header.Get("identity_role")
		got.id = r.Header.Get("identity_id")
		raw, _ := io.ReadAll(r.Body)
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &got.body); err != nil {
				t.Errorf("body: %v", err)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"requestId":"req-1","state":"queued"}`))
	}))
	defer srv.Close()

	client, err := NewClient(srv.URL, "agent", "deployment-mcp", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Do(context.Background(), Request{
		Method:   http.MethodPost,
		Path:     "/api/deploys",
		Identity: Identity{ID: "agent-42"},
		Body:     map[string]any{"serviceId": "web-cursor", "deployment": "deployment-abc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.method != http.MethodPost || got.path != "/api/deploys" {
		t.Fatalf("request = %s %s", got.method, got.path)
	}
	if got.role != "agent" || got.id != "agent-42" {
		t.Fatalf("identity = %s:%s", got.role, got.id)
	}
	if got.body["serviceId"] != "web-cursor" || got.body["deployment"] != "deployment-abc" {
		t.Fatalf("body = %#v", got.body)
	}
	if res.Status != http.StatusAccepted {
		t.Fatalf("status = %d", res.Status)
	}
	if !strings.Contains(formatBody(res.Status, res.Body), `"requestId": "req-1"`) {
		t.Fatalf("body = %s", res.Body)
	}
}

func TestSegmentRejectsTraversal(t *testing.T) {
	for _, raw := range []string{"", "../deploys", "a/b", `a\b`, "a?b"} {
		if _, err := segment("serviceId", raw); err == nil {
			t.Fatalf("segment(%q) should fail", raw)
		}
	}
	got, err := segment("serviceId", "web-cursor")
	if err != nil || got != "web-cursor" {
		t.Fatalf("got %q err %v", got, err)
	}
}
