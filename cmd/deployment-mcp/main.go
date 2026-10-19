package main

import (
	"fmt"
	"os"

	"github.com/kaulie/agent-control-plane-deployment/mcp"
)

func main() {
	addr, rest, err := deploymcp.SplitHTTPFlag(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "deployment-mcp: %v\n", err)
		os.Exit(2)
	}
	if addr == "" {
		start, code, text := deploymcp.PrepareCLI(rest, os.Stdin)
		if !start {
			exitCLI(code, text)
		}
		if err := deploymcp.ServeStdio(); err != nil {
			fmt.Fprintf(os.Stderr, "deployment-mcp: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if len(rest) > 0 {
		start, code, text := deploymcp.PrepareCLI(rest, nil)
		if !start {
			exitCLI(code, text)
		}
	}
	if err := deploymcp.ListenAndServe(addr); err != nil {
		fmt.Fprintf(os.Stderr, "deployment-mcp: %v\n", err)
		os.Exit(1)
	}
}

func exitCLI(code int, text string) {
	if code == 0 {
		fmt.Fprint(os.Stdout, text)
	} else {
		fmt.Fprint(os.Stderr, text)
	}
	os.Exit(code)
}
