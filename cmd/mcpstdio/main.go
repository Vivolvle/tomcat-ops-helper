package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"time"
)

// mcpstdio is a stdio MCP bridge that talks to server.exe's HTTP /mcp/*
// endpoints. It is meant to be launched by MCP Inspector, e.g.:
//
//	npx @modelcontextprotocol/inspector go run ./cmd/mcpstdio
//
// All logging goes to stderr so stdout is pure JSON-RPC.
func main() {
	urlFlag := flag.String("url", "", "HTTP base URL of server.exe (default $MCP_HTTP_URL or http://127.0.0.1:7777)")
	clientFlag := flag.String("client", "", "Helper client_id to pin to (default $MCP_CLIENT_ID or auto-detect when exactly one helper is connected)")
	pollFlag := flag.Duration("poll-interval", defaultPollInterval,
		"How often to re-check /mcp/tools and push notifications/tools/list_changed. "+
			"Use 0 to disable polling. Can also be set via MCP_POLL_INTERVAL env var (e.g. 2s, 500ms).")
	flag.Parse()

	if err := run(*urlFlag, *clientFlag, *pollFlag, os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "mcpstdio: %v\n", err)
		os.Exit(1)
	}
}

func run(urlFlag, clientFlag string, pollInterval time.Duration, stdin io.Reader, stdout, stderr io.Writer) error {
	target := urlFlag
	if target == "" {
		target = os.Getenv("MCP_HTTP_URL")
	}
	if target == "" {
		target = "http://127.0.0.1:7777"
	}

	cid := clientFlag
	if cid == "" {
		cid = os.Getenv("MCP_CLIENT_ID")
	}

	if pollInterval <= 0 {
		if env := os.Getenv("MCP_POLL_INTERVAL"); env != "" {
			if d, err := time.ParseDuration(env); err == nil {
				pollInterval = d
			}
		}
	}

	if cid != "" {
		fmt.Fprintf(stderr, "mcpstdio: bridge → %s (client_id=%s) poll=%s\n", target, cid, pollInterval)
	} else {
		fmt.Fprintf(stderr, "mcpstdio: bridge → %s (auto-detect client_id) poll=%s\n", target, pollInterval)
	}

	b := NewBridge(target, cid)
	b.PollInterval = pollInterval
	return b.Serve(stdin, stdout, stderr)
}
