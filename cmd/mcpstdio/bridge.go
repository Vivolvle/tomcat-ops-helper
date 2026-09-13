// Package main is a tiny MCP-over-stdio bridge that talks to the project's
// server.exe HTTP bridge. It speaks the Model Context Protocol on stdin/stdout
// (newline-delimited JSON-RPC 2.0) so that MCP Inspector (or any other stdio
// MCP client) can drive the helper through the existing /mcp/* HTTP endpoints.
//
// Only a small set of MCP methods are implemented:
//
//	initialize           — returns a fixed serverInfo, echoes protocolVersion
//	notifications/*      — accepted silently, no response written
//	tools/list           — GETs /mcp/tools, flattens to {tools:[...]}
//	tools/call           — POSTs the request body to /mcp/call, passes response through
//	ping                 — returns {}
//
// Anything else returns a JSON-RPC -32601 method-not-found error.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// supportedProtocolVersions is the list of MCP protocol versions this bridge
// is willing to advertise. The most recent comes first; the bridge echoes
// whatever the client sends so negotiation can downgrade.
var supportedProtocolVersions = []string{
	"2025-06-18",
	"2025-03-26",
	"2024-11-05",
}

// Bridge is a stdio→HTTP MCP relay.
type Bridge struct {
	URL      string
	ClientID string
	HTTP     *http.Client

	serverName    string
	serverVersion string
}

// NewBridge returns a Bridge wired with sensible defaults: a 60s HTTP client
// and the project's standard server identity. Override fields directly after
// construction if you need different behavior in tests.
func NewBridge(rawURL, clientID string) *Bridge {
	return &Bridge{
		URL:           strings.TrimRight(rawURL, "/"),
		ClientID:      clientID,
		HTTP:          &http.Client{Timeout: 60 * time.Second},
		serverName:    "tomcat-ops-helper-bridge",
		serverVersion: "0.1.0",
	}
}

// Serve reads JSON-RPC frames one per line from stdin and writes responses
// to stdout until EOF. All diagnostics go to stderr so stdout stays pure
// JSON-RPC for the MCP client.
//
// Serve only returns an error for I/O failures on the underlying streams or
// the HTTP client. Malformed frames become JSON-RPC error responses and do
// not stop the loop.
func (b *Bridge) Serve(stdin io.Reader, stdout, stderr io.Writer) error {
	in := bufio.NewScanner(stdin)
	in.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // up to 4 MiB per frame

	out := json.NewEncoder(stdout)
	out.SetEscapeHTML(false)

	for in.Scan() {
		line := bytesTrim(in.Bytes())
		if len(line) == 0 {
			continue
		}
		b.dispatch(line, out, stderr)
	}
	if err := in.Err(); err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	return nil
}

// dispatch parses one frame, routes by method, and writes the response.
func (b *Bridge) dispatch(raw []byte, out *json.Encoder, stderr io.Writer) {
	var req jsonrpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		b.writeErr(out, nil, -32700, "parse error", err.Error())
		return
	}
	if req.JSONRPC != "" && req.JSONRPC != "2.0" {
		b.writeErr(out, req.ID, -32600, "invalid jsonrpc version", req.JSONRPC)
		return
	}
	isNotification := strings.HasPrefix(req.Method, "notifications/")
	if req.ID == nil && !isNotification {
		b.writeErr(out, nil, -32600, "missing id", req.Method)
		return
	}

	switch req.Method {
	case "initialize":
		b.handleInitialize(&req, out)
	case "notifications/initialized", "notifications/cancelled", "notifications/progress":
		// Fire-and-forget; per spec, no response.
		return
	case "ping":
		b.writeOK(out, req.ID, map[string]any{})
	case "tools/list":
		b.handleToolsList(&req, out, stderr)
	case "tools/call":
		b.handleToolsCall(&req, out, stderr)
	default:
		b.writeErr(out, req.ID, -32601, "method not found", req.Method)
	}
}

// handleInitialize answers the MCP handshake. The protocolVersion from the
// client is echoed back unchanged so the client can pick a compatible one;
// capabilities advertises tools only.
func (b *Bridge) handleInitialize(req *jsonrpcRequest, out *json.Encoder) {
	var p struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
		ClientInfo      map[string]any `json:"clientInfo"`
	}
	_ = json.Unmarshal(req.Params, &p) // best-effort

	ver := p.ProtocolVersion
	if ver == "" {
		ver = supportedProtocolVersions[0]
	}

	b.writeOK(out, req.ID, map[string]any{
		"protocolVersion": ver,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo": map[string]any{
			"name":    b.serverName,
			"version": b.serverVersion,
		},
	})
}

// handleToolsList fetches the helper's advertised tools and returns them in
// MCP's {tools:[...]} shape. When the upstream server reports more than one
// connected helper, ClientID must be set; otherwise the single helper is
// auto-selected.
func (b *Bridge) handleToolsList(req *jsonrpcRequest, out *json.Encoder, stderr io.Writer) {
	tools, err := b.fetchTools(context.Background(), stderr)
	if err != nil {
		b.writeErr(out, req.ID, -32603, "internal error", err.Error())
		return
	}
	b.writeOK(out, req.ID, map[string]any{"tools": tools})
}

// handleToolsCall forwards the call to /mcp/call. The MCP stdio request's
// params already carry name/arguments; the bridge injects client_id
// (resolved from flag/env or by auto-picking the single helper) and forwards
// the JSON-RPC envelope. The /mcp/call handler returns a fully-formed
// JSON-RPC response whose `result` we relay byte-for-byte.
func (b *Bridge) handleToolsCall(req *jsonrpcRequest, out *json.Encoder, stderr io.Writer) {
	var p map[string]json.RawMessage
	if err := json.Unmarshal(req.Params, &p); err != nil {
		b.writeErr(out, req.ID, -32602, "invalid params", err.Error())
		return
	}
	if _, ok := p["name"]; !ok {
		b.writeErr(out, req.ID, -32602, "missing tool name", "")
		return
	}

	cid, err := b.resolveClientID(context.Background(), stderr)
	if err != nil {
		b.writeErr(out, req.ID, -32004, "client_id unresolved", err.Error())
		return
	}
	quoted, err := json.Marshal(cid)
	if err != nil {
		b.writeErr(out, req.ID, -32603, "internal error", err.Error())
		return
	}
	p["client_id"] = quoted

	forwarded, err := json.Marshal(jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      req.ID,
		Method:  "tools/call",
		Params:  mustMarshal(p),
	})
	if err != nil {
		b.writeErr(out, req.ID, -32603, "internal error", err.Error())
		return
	}

	httpReq, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		b.URL+"/mcp/call", strings.NewReader(string(forwarded)))
	if err != nil {
		b.writeErr(out, req.ID, -32603, "internal error", err.Error())
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := b.HTTP.Do(httpReq)
	if err != nil {
		b.writeErr(out, req.ID, -32603, "upstream unreachable", err.Error())
		return
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		b.writeErr(out, req.ID, -32603, "read upstream", err.Error())
		return
	}
	// /mcp/call returns a JSON-RPC envelope (HTTP 200 for both success and
	// application errors). Extract its `result` or `error` and rewrap it as
	// our own JSON-RPC response so the MCP client sees a single, well-formed
	// envelope on this stream.
	trimmed := bytesTrim(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		b.writeErr(out, req.ID, -32603, "upstream returned non-JSON",
			fmt.Sprintf("status=%d body=%q", resp.StatusCode, string(trimmed)))
		return
	}
	var upstream struct {
		Result json.RawMessage `json:"result"`
		Error  *jsonrpcError   `json:"error"`
	}
	if err := json.Unmarshal(trimmed, &upstream); err != nil {
		b.writeErr(out, req.ID, -32603, "decode upstream envelope",
			fmt.Sprintf("status=%d body=%q", resp.StatusCode, string(trimmed)))
		return
	}
	resp2 := jsonrpcResponse{JSONRPC: "2.0", ID: req.ID}
	if upstream.Error != nil {
		resp2.Error = upstream.Error
	} else if len(upstream.Result) > 0 {
		resp2.Result = upstream.Result
	} else {
		b.writeErr(out, req.ID, -32603, "upstream envelope missing result/error",
			string(trimmed))
		return
	}
	if err := out.Encode(resp2); err != nil {
		fmt.Fprintf(stderr, "bridge: write response: %v\n", err)
	}
}

// fetchTools returns the tool list for whichever helper the bridge is
// pinned to. It calls /mcp/tools?client_id=X when ClientID is known;
// otherwise it pulls the aggregated view and picks the single client
// (rejecting ambiguity).
func (b *Bridge) fetchTools(ctx context.Context, stderr io.Writer) ([]toolMeta, error) {
	if b.ClientID != "" {
		return b.fetchToolsFor(ctx, b.ClientID)
	}
	body, err := b.getJSON(ctx, "/mcp/tools")
	if err != nil {
		return nil, err
	}
	var agg struct {
		Clients []struct {
			ClientID string     `json:"client_id"`
			Tools    []toolMeta `json:"tools"`
		} `json:"clients"`
	}
	if err := json.Unmarshal(body, &agg); err != nil {
		return nil, fmt.Errorf("decode /mcp/tools: %w", err)
	}
	switch len(agg.Clients) {
	case 0:
		return nil, fmt.Errorf("no helper connected to %s", b.URL)
	case 1:
		return agg.Clients[0].Tools, nil
	default:
		fmt.Fprintf(stderr,
			"bridge: %d helpers connected; set MCP_CLIENT_ID to disambiguate\n",
			len(agg.Clients))
		return nil, fmt.Errorf(
			"multiple helpers connected (%d); set MCP_CLIENT_ID",
			len(agg.Clients))
	}
}

func (b *Bridge) fetchToolsFor(ctx context.Context, cid string) ([]toolMeta, error) {
	body, err := b.getJSON(ctx, "/mcp/tools?client_id="+url.QueryEscape(cid))
	if err != nil {
		return nil, err
	}
	var resp struct {
		Tools []toolMeta `json:"tools"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode /mcp/tools: %w", err)
	}
	return resp.Tools, nil
}

// resolveClientID returns the helper id to stamp on outgoing calls. It
// prefers the configured ClientID; otherwise it auto-picks the single helper.
func (b *Bridge) resolveClientID(ctx context.Context, stderr io.Writer) (string, error) {
	if b.ClientID != "" {
		return b.ClientID, nil
	}
	body, err := b.getJSON(ctx, "/mcp/tools")
	if err != nil {
		return "", err
	}
	var agg struct {
		Clients []struct {
			ClientID string `json:"client_id"`
		} `json:"clients"`
	}
	if err := json.Unmarshal(body, &agg); err != nil {
		return "", fmt.Errorf("decode /mcp/tools: %w", err)
	}
	switch len(agg.Clients) {
	case 0:
		return "", fmt.Errorf("no helper connected to %s", b.URL)
	case 1:
		return agg.Clients[0].ClientID, nil
	default:
		fmt.Fprintf(stderr,
			"bridge: %d helpers connected; set MCP_CLIENT_ID to disambiguate\n",
			len(agg.Clients))
		return "", fmt.Errorf(
			"multiple helpers connected (%d); set MCP_CLIENT_ID",
			len(agg.Clients))
	}
}

func (b *Bridge) getJSON(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.URL+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d: %s",
			path, resp.StatusCode, string(body))
	}
	return body, nil
}

// writeOK writes a JSON-RPC success response. A broken stdout is logged and
// swallowed; the bridge cannot meaningfully recover.
func (b *Bridge) writeOK(out *json.Encoder, id json.RawMessage, result any) {
	if err := out.Encode(jsonrpcResponse{
		JSONRPC: "2.0", ID: id, Result: result,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "bridge: write result: %v\n", err)
	}
}

// writeErr writes a JSON-RPC error response.
func (b *Bridge) writeErr(out *json.Encoder, id json.RawMessage, code int, msg, data string) {
	if err := out.Encode(jsonrpcResponse{
		JSONRPC: "2.0", ID: id,
		Error: &jsonrpcError{Code: code, Message: msg, Data: data},
	}); err != nil {
		fmt.Fprintf(os.Stderr, "bridge: write error: %v\n", err)
	}
}

// jsonrpcRequest is the subset of JSON-RPC 2.0 this bridge accepts.
type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data,omitempty"`
}

// toolMeta mirrors the wire format produced by /mcp/tools. Defined locally so
// this cmd has no dependency on internal/protocol.
type toolMeta struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// mustMarshal marshals v and returns json.RawMessage. If marshalling fails
// the caller has already broken the JSON-RPC envelope; returning null keeps
// the outer envelope parseable rather than corrupting it.
func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}

// bytesTrim trims ASCII whitespace from both ends.
func bytesTrim(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && isSpace(b[start]) {
		start++
	}
	for end > start && isSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
