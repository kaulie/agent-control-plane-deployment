package deploymcp

import (
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// NewHandler serves Streamable HTTP MCP. Each POST gets its own server so
// identity headers on that request apply to the control-plane calls it makes.
// The handler is stateless: any source machine can call it without a sticky session.
func NewHandler(base *Client) http.Handler {
	return mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		if base == nil {
			return nil
		}
		return newMCP(base.withRequestIdentity(r))
	}, &mcp.StreamableHTTPOptions{Stateless: true})
}

// LoopbackHandler serves MCP that calls this control plane on host:port.
// A wildcard listen address (0.0.0.0, ::) is dialed as 127.0.0.1.
func LoopbackHandler(host string, port int) (http.Handler, string, error) {
	upstream, err := NewClientFromEnv()
	if err != nil {
		return nil, "", err
	}
	id := upstream.DefaultIdentity()
	baseURL := SelfBaseURL(host, port)
	client, err := NewClient(baseURL, id.Role, id.ID, upstream.Timeout())
	if err != nil {
		return nil, "", err
	}
	return NewHandler(client), baseURL, nil
}

// ListenAndServe runs a standalone HTTP MCP that proxies to DEPLOYMENT_URL.
func ListenAndServe(addr string) error {
	client, err := NewClientFromEnv()
	if err != nil {
		return err
	}
	log.Printf("deployment-mcp HTTP %s, upstream %s", addr, client.BaseURL())
	return http.ListenAndServe(addr, NewHandler(client))
}

// SelfBaseURL is the URL this process should use to call its own API.
func SelfBaseURL(host string, port int) string {
	switch strings.TrimSpace(host) {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(port))
}

// Timeout is the HTTP client timeout used for control-plane calls.
func (c *Client) Timeout() time.Duration {
	if c != nil && c.http != nil && c.http.Timeout > 0 {
		return c.http.Timeout
	}
	return defaultTimeout
}

// withRequestIdentity copies the client and, when the MCP request carries
// identity headers, uses those as the caller recorded on deploy/release.
func (c *Client) withRequestIdentity(r *http.Request) *Client {
	if c == nil || r == nil {
		return c
	}
	role := strings.TrimSpace(r.Header.Get("identity_role"))
	id := strings.TrimSpace(r.Header.Get("identity_id"))
	if role == "" && id == "" {
		return c
	}
	next := *c
	if role != "" {
		next.identityRole = role
	}
	if id != "" {
		next.identityID = id
	}
	return &next
}
