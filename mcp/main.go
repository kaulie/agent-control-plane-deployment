// Command deployment-mcp exposes the deployment control plane HTTP API as a
// stdio MCP server. It does not deploy anything by itself: tool calls are
// requests to the platform at DEPLOYMENT_URL (default http://127.0.0.1:4220).
//
// /restart/notify and /restart/poll stay internal. Those drain the control
// plane process and are not agent tools.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const version = "1.0.0"

func main() {
	client, err := NewClientFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "deployment-mcp: %v\n", err)
		os.Exit(2)
	}
	if err := newMCP(client).Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintf(os.Stderr, "deployment-mcp: %v\n", err)
		os.Exit(1)
	}
}

func newMCP(client *Client) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "deployment",
		Version: version,
	}, nil)
	(&skill{client: client}).register(srv)
	return srv
}
