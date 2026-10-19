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

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSelfBaseURL(t *testing.T) {
	if got := SelfBaseURL("0.0.0.0", 4220); got != "http://127.0.0.1:4220" {
		t.Fatalf("wildcard = %s", got)
	}
	if got := SelfBaseURL("10.0.0.8", 4220); got != "http://10.0.0.8:4220" {
		t.Fatalf("specific = %s", got)
	}
}

func TestHTTPReleaseForwardsIdentity(t *testing.T) {
	var got struct {
		path string
		role string
		id   string
		body map[string]any
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		got.role = r.Header.Get("identity_role")
		got.id = r.Header.Get("identity_id")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got.body)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"requestId":"pipe-9"}`))
	}))
	defer api.Close()

	client, err := NewClient(api.URL, "agent", "fallback", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	mcpHTTP := httptest.NewServer(NewHandler(client))
	defer mcpHTTP.Close()

	ctx := context.Background()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(
		ctx,
		&mcp.StreamableClientTransport{
			Endpoint:             mcpHTTP.URL,
			DisableStandaloneSSE: true,
			HTTPClient: &http.Client{Transport: headerTransport{
				base: http.DefaultTransport,
				role: "user",
				id:   "user_9",
			}},
		},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "deployment_release",
		Arguments: map[string]any{"serviceId": "web-cursor", "ref": "main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("tool error: %+v", res)
	}
	if got.path != "/api/deploy-notify" || got.role != "user" || got.id != "user_9" {
		t.Fatalf("upstream = %s %s:%s", got.path, got.role, got.id)
	}
	if got.body["serviceId"] != "web-cursor" || got.body["ref"] != "main" {
		t.Fatalf("body = %#v", got.body)
	}
	text := ""
	if len(res.Content) > 0 {
		if c, ok := res.Content[0].(*mcp.TextContent); ok {
			text = c.Text
		}
	}
	if !strings.Contains(text, "pipe-9") {
		t.Fatalf("result = %s", text)
	}
}

type headerTransport struct {
	base http.RoundTripper
	role string
	id   string
}

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("identity_role", h.role)
	r.Header.Set("identity_id", h.id)
	return h.base.RoundTrip(r)
}
