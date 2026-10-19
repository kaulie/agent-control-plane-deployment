// Package main is the deployment-mcp stdio server. client.go speaks the
// deployment control plane's HTTP API (default http://127.0.0.1:4220).
package deploymcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultBaseURL = "http://127.0.0.1:4220"
	defaultRole    = "agent"
	defaultID      = "deployment-mcp"
	defaultTimeout = 60 * time.Second
	maxBodyBytes   = 256 * 1024

	envBaseURL = "DEPLOYMENT_URL"
	envRole    = "IDENTITY_ROLE"
	envID      = "IDENTITY_ID"
	envTimeout = "DEPLOYMENT_HTTP_TIMEOUT"
)

// Client calls the deployment control plane.
type Client struct {
	baseURL      string
	identityRole string
	identityID   string
	http         *http.Client
}

// NewClientFromEnv reads DEPLOYMENT_URL, IDENTITY_ROLE, IDENTITY_ID, and
// DEPLOYMENT_HTTP_TIMEOUT (Go duration, default 60s).
func NewClientFromEnv() (*Client, error) {
	timeout := defaultTimeout
	if raw := strings.TrimSpace(os.Getenv(envTimeout)); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", envTimeout, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("%s must be positive", envTimeout)
		}
		timeout = d
	}
	role := strings.TrimSpace(os.Getenv(envRole))
	if role == "" {
		role = defaultRole
	}
	id := strings.TrimSpace(os.Getenv(envID))
	if id == "" {
		id = defaultID
	}
	base := strings.TrimSpace(os.Getenv(envBaseURL))
	if base == "" {
		base = defaultBaseURL
	}
	return NewClient(base, role, id, timeout)
}

// NewClient checks the base URL and builds an HTTP client. role and id are the
// default identity headers (identity_role / identity_id). A tool call may
// override them.
func NewClient(baseURL, role, id string, timeout time.Duration) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("DEPLOYMENT_URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("DEPLOYMENT_URL must be http or https, got %q", baseURL)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("DEPLOYMENT_URL is missing a host: %q", baseURL)
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Client{
		baseURL:      baseURL,
		identityRole: strings.TrimSpace(role),
		identityID:   strings.TrimSpace(id),
		http:         &http.Client{Timeout: timeout},
	}, nil
}

// Identity is the role/id pair that will be sent as identity headers.
type Identity struct {
	Role string
	ID   string
}

// DefaultIdentity returns the identity configured on the client.
func (c *Client) DefaultIdentity() Identity {
	return Identity{Role: c.identityRole, ID: c.identityID}
}

// BaseURL is the control-plane origin, without a trailing slash.
func (c *Client) BaseURL() string { return c.baseURL }

// Request is one HTTP call to the control plane.
type Request struct {
	Method string
	Path   string
	Query  url.Values
	Body   any
	// Identity overrides the process default. Empty role or id falls back
	// to IDENTITY_ROLE / IDENTITY_ID (or agent / deployment-mcp).
	Identity Identity
}

// Response is the raw HTTP result. Body is capped at maxBodyBytes.
type Response struct {
	Status int
	Body   []byte
}

// Do performs the request. A non-2xx status is still returned as a Response
// (the caller decides how to surface it). Transport failures return an error.
func (c *Client) Do(ctx context.Context, req Request) (Response, error) {
	identity := req.Identity
	if identity.Role == "" {
		identity.Role = c.identityRole
	}
	if identity.ID == "" {
		identity.ID = c.identityID
	}
	var payload io.Reader
	if req.Body != nil {
		raw, err := json.Marshal(req.Body)
		if err != nil {
			return Response{}, err
		}
		payload = bytes.NewReader(raw)
	}
	u, err := c.resolve(req.Path, req.Query)
	if err != nil {
		return Response{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, u, payload)
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", "deployment-mcp/1.0.0")
	if req.Body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	if identity.Role != "" {
		httpReq.Header.Set("identity_role", identity.Role)
	}
	if identity.ID != "" {
		httpReq.Header.Set("identity_id", identity.ID)
	}
	res, err := c.http.Do(httpReq)
	if err != nil {
		return Response{}, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxBodyBytes+1))
	if err != nil {
		return Response{}, err
	}
	truncated := false
	if len(body) > maxBodyBytes {
		body = body[:maxBodyBytes]
		truncated = true
	}
	if truncated {
		body = append(body, []byte("\n…(truncated)")...)
	}
	return Response{Status: res.StatusCode, Body: body}, nil
}

func (c *Client) resolve(path string, q url.Values) (string, error) {
	if !strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("path must start with /: %q", path)
	}
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return "", err
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	if len(q) > 0 {
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// segment checks a single URL path segment and returns it escaped.
func segment(name, raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	if strings.Contains(v, "..") || strings.ContainsAny(v, "/\\?#\r\n") {
		return "", fmt.Errorf("%s contains illegal characters", name)
	}
	return url.PathEscape(v), nil
}

func setQuery(q url.Values, key, value string) {
	value = strings.TrimSpace(value)
	if value != "" {
		q.Set(key, value)
	}
}

func setQueryInt(q url.Values, key string, value *int) {
	if value != nil {
		q.Set(key, strconv.Itoa(*value))
	}
}

func formatBody(status int, body []byte) string {
	pretty := prettyJSON(body)
	if pretty == "" {
		pretty = "(empty body)"
	}
	return fmt.Sprintf("HTTP %d\n%s", status, pretty)
}

func prettyJSON(body []byte) string {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, body, "", "  "); err != nil {
		return string(body)
	}
	return buf.String()
}
