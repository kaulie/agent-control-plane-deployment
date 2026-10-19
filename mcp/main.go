// Package deploymcp exposes the deployment control plane HTTP API as an MCP
// server. Tool calls are requests to the platform (default
// http://127.0.0.1:4220). The process does not deploy anything itself.
//
// /restart/notify and /restart/poll stay internal. Those drain the control
// plane process and are not agent tools.
//
// Two transports:
//   - stdio, for a client on this machine (Cursor command)
//   - Streamable HTTP, for clients on other machines (mounted at /mcp on the
//     control plane, or via `deployment-mcp --http`)
package deploymcp

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const version = "1.0.0"

// ServeStdio runs the MCP server on stdin/stdout until the client disconnects.
func ServeStdio() error {
	client, err := NewClientFromEnv()
	if err != nil {
		return err
	}
	return newMCP(client).Run(context.Background(), &mcp.StdioTransport{})
}

func newMCP(client *Client) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "deployment",
		Version: version,
	}, nil)
	(&skill{client: client}).register(srv)
	return srv
}
