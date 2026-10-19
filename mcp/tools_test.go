package main

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

func TestConfigureBodyOmitsUnsetFields(t *testing.T) {
	port := 4220
	body, err := (configureServiceArgs{ServiceID: "web-cursor", Port: &port}).body()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := body["runtimeDir"]; ok {
		t.Fatalf("runtimeDir should be omitted: %#v", body)
	}
	if body["port"] != 4220 {
		t.Fatalf("port = %#v", body["port"])
	}
	env := map[string]string{"FOO": "bar"}
	_, err = (configureServiceArgs{RuntimeEnv: env, ClearRuntimeEnv: true}).body()
	if err == nil {
		t.Fatal("clearRuntimeEnv together with runtimeEnv should fail")
	}
	cleared, err := (configureServiceArgs{ClearRuntimeEnv: true}).body()
	if err != nil {
		t.Fatal(err)
	}
	raw, ok := cleared["runtimeEnv"].(map[string]string)
	if !ok || len(raw) != 0 {
		t.Fatalf("runtimeEnv = %#v", cleared["runtimeEnv"])
	}
}

func TestDeployListQuery(t *testing.T) {
	page := 2
	q := (deployListArgs{ServiceID: "web-cursor", State: "running", Page: &page}).query()
	if q.Get("serviceId") != "web-cursor" || q.Get("state") != "running" || q.Get("page") != "2" {
		t.Fatalf("query = %s", q.Encode())
	}
	if q.Get("deployment") != "" {
		t.Fatal("empty deployment should be omitted")
	}
}

func TestReleaseOmitsUnsetProxy(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/deploy-notify" {
			t.Errorf("path = %s", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("body: %v", err)
		}
		if r.Header.Get("identity_role") != "user" || r.Header.Get("identity_id") != "user_9" {
			t.Errorf("identity %s %s", r.Header.Get("identity_role"), r.Header.Get("identity_id"))
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"requestId":"pipe-1"}`))
	}))
	defer srv.Close()

	sk, err := testSkill(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	res, _, err := sk.release(context.Background(), nil, releaseArgs{
		ServiceID:    "web-cursor",
		Ref:          "main",
		IdentityRole: "user",
		IdentityID:   "user_9",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("result error: %s", resultText(t, res))
	}
	if _, ok := body["useProxy"]; ok {
		t.Fatalf("useProxy should be omitted: %#v", body)
	}
	if body["serviceId"] != "web-cursor" || body["ref"] != "main" {
		t.Fatalf("body = %#v", body)
	}
}

func TestServiceIDTraversalDoesNotCallAPI(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	defer srv.Close()
	sk, err := testSkill(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = sk.getService(context.Background(), nil, serviceIDArgs{ServiceID: "../deploys"})
	if err == nil {
		t.Fatal("expected path error")
	}
	if called {
		t.Fatal("API was called")
	}
}

func TestMCPToolsRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/health":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case r.URL.Path == "/api/deploys" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"missing identity"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client, err := NewClient(srv.URL, "agent", "deployment-mcp", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	left, right := mcp.NewInMemoryTransports()
	if _, err := newMCP(client).Connect(ctx, left, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcpClient.Connect(ctx, right, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range listed.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{
		"deployment_client",
		"deployment_health",
		"deployment_meta",
		"deployment_inventory",
		"deployment_list_services",
		"deployment_get_service",
		"deployment_service_history",
		"deployment_configure_service",
		"deployment_delete_service_config",
		"deployment_move_service_history",
		"deployment_list_artifacts",
		"deployment_get_artifact",
		"deployment_scan_artifacts",
		"deployment_list_deploys",
		"deployment_get_deploy",
		"deployment_deploy_events",
		"deployment_deploy",
		"deployment_list_pipelines",
		"deployment_get_pipeline",
		"deployment_pipeline_events",
		"deployment_release",
	} {
		if !names[want] {
			t.Errorf("missing tool %s", want)
		}
	}
	if len(names) != 21 {
		t.Fatalf("tool count = %d, names = %v", len(names), names)
	}

	health, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "deployment_health",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if health.IsError || !strings.Contains(resultText(t, health), `"ok": true`) {
		t.Fatalf("health = error:%v text:%s", health.IsError, resultText(t, health))
	}

	info, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "deployment_client",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(t, info)
	if !strings.Contains(text, srv.URL) || !strings.Contains(text, "deployment-mcp") {
		t.Fatalf("client info = %s", text)
	}

	denied, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "deployment_deploy",
		Arguments: map[string]any{"serviceId": "web-cursor", "deployment": "deployment-abc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !denied.IsError || !strings.Contains(resultText(t, denied), "missing identity") {
		t.Fatalf("deploy = error:%v text:%s", denied.IsError, resultText(t, denied))
	}
}

func testSkill(base string) (*skill, error) {
	client, err := NewClient(base, "agent", "deployment-mcp", time.Second)
	if err != nil {
		return nil, err
	}
	return &skill{client: client}, nil
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		t.Fatal("empty tool result")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content type %T", res.Content[0])
	}
	return text.Text
}
