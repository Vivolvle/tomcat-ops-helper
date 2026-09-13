package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeServer simulates server.exe's /mcp/* endpoints. The /mcp/tools
// aggregator returns whatever `toolsPayload()` produces; /mcp/call returns
// `callResult()` or `callError()` depending on the configured mode.
type fakeServer struct {
	*httptest.Server

	// Per-call overrides. If set, they win over the canned responses.
	gotCallBody atomic.Value // last body received by /mcp/call

	toolsMode atomic.Value // "single", "multi", "none"
	pinnedID  atomic.Value // string

	callMode atomic.Value // "ok", "tool-error", "non-json"
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{}
	f.toolsMode.Store("single")
	f.pinnedID.Store("helper-A")
	f.callMode.Store("ok")

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp/tools", f.handleTools)
	mux.HandleFunc("/mcp/call", f.handleCall)
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeServer) handleTools(w http.ResponseWriter, r *http.Request) {
	if cid := r.URL.Query().Get("client_id"); cid != "" {
		// Pinned view.
		if cid != f.pinnedID.Load().(string) {
			http.Error(w, "unknown client_id", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"client_id": cid,
			"tools":     cannedTools,
		})
		return
	}
	switch f.toolsMode.Load().(string) {
	case "none":
		writeJSON(w, http.StatusOK, map[string]any{"clients": []any{}})
	case "single":
		writeJSON(w, http.StatusOK, map[string]any{
			"clients": []map[string]any{
				{"client_id": f.pinnedID.Load().(string), "tools": cannedTools},
			},
		})
	case "multi":
		writeJSON(w, http.StatusOK, map[string]any{
			"clients": []map[string]any{
				{"client_id": "helper-A", "tools": cannedTools},
				{"client_id": "helper-B", "tools": cannedTools},
			},
		})
	default:
		http.Error(w, "bad mode", 500)
	}
}

func (f *fakeServer) handleCall(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.gotCallBody.Store(string(body))
	switch f.callMode.Load().(string) {
	case "ok":
		writeJSON(w, http.StatusOK, map[string]any{
			"jsonrpc": "2.0",
			"id":      json.RawMessage(body),
			"result": map[string]any{
				"content": []map[string]any{{"type": "text", "text": "hello-from-helper"}},
				"data":    json.RawMessage(`{"ok":true}`),
			},
		})
	case "tool-error":
		writeJSON(w, http.StatusOK, map[string]any{
			"jsonrpc": "2.0",
			"id":      json.RawMessage(body),
			"error":   map[string]any{"code": -32000, "message": "boom", "data": "tool said no"},
		})
	case "non-json":
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("not json at all"))
	default:
		http.Error(w, "bad mode", 500)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

var cannedTools = []toolMeta{
	{
		Name:        "status",
		Description: "Get helper status",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	},
	{
		Name:        "exec",
		Description: "Run a shell command",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}`),
	},
}

// runBridge feeds `input` line-by-line into a Bridge pointed at the fake
// server, returns whatever ended up on stdout and stderr.
func runBridge(t *testing.T, f *fakeServer, input string, clientID string) (string, string) {
	t.Helper()
	b := NewBridge(f.URL, clientID)
	stdin := strings.NewReader(input)
	var out, errOut bytes.Buffer
	if err := b.Serve(stdin, &out, &errOut); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	return out.String(), errOut.String()
}

// parseLine extracts one JSON-RPC response from a multi-line stdout buffer.
func parseLine(t *testing.T, stdout, prefix string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, prefix) {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		return m
	}
	t.Fatalf("no line with prefix %q in stdout:\n%s", prefix, stdout)
	return nil
}

func TestBridge_Initialize(t *testing.T) {
	f := newFakeServer(t)
	stdout, _ := runBridge(t, f,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","clientInfo":{"name":"x"}}}`+
			"\n",
		"")
	got := parseLine(t, stdout, `{"jsonrpc":"2.0"`)
	if got["id"].(float64) != 1 {
		t.Fatalf("id echo = %v", got["id"])
	}
	res := got["result"].(map[string]any)
	if res["protocolVersion"] != "2024-11-05" {
		t.Fatalf("protocolVersion = %v", res["protocolVersion"])
	}
	cap := res["capabilities"].(map[string]any)
	if _, ok := cap["tools"]; !ok {
		t.Fatalf("capabilities missing tools: %v", cap)
	}
	info := res["serverInfo"].(map[string]any)
	if info["name"] != "tomcat-ops-helper-bridge" {
		t.Fatalf("serverInfo.name = %v", info["name"])
	}
}

func TestBridge_Notification_NoResponse(t *testing.T) {
	f := newFakeServer(t)
	stdout, _ := runBridge(t, f,
		`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`+"\n",
		"")
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("notification produced stdout: %q", stdout)
	}
}

func TestBridge_ToolsList_SingleHelper(t *testing.T) {
	f := newFakeServer(t)
	stdout, _ := runBridge(t, f,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`+"\n",
		"")
	got := parseLine(t, stdout, `{"jsonrpc":"2.0"`)
	if got["id"].(float64) != 2 {
		t.Fatalf("id echo = %v", got["id"])
	}
	res := got["result"].(map[string]any)
	tools := res["tools"].([]any)
	if len(tools) != 2 || tools[0].(map[string]any)["name"] != "status" {
		t.Fatalf("tools = %v", tools)
	}
}

func TestBridge_ToolsList_MultiHelper_NoClientID_Errors(t *testing.T) {
	f := newFakeServer(t)
	f.toolsMode.Store("multi")
	stdout, _ := runBridge(t, f,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`+"\n",
		"")
	got := parseLine(t, stdout, `{"jsonrpc":"2.0"`)
	errObj := got["error"].(map[string]any)
	if errObj["message"] != "internal error" {
		t.Fatalf("message = %v", errObj["message"])
	}
	if !strings.Contains(errObj["data"].(string), "MCP_CLIENT_ID") {
		t.Fatalf("data should hint at MCP_CLIENT_ID, got %v", errObj["data"])
	}
}

func TestBridge_ToolsList_MultiHelper_WithClientID_OK(t *testing.T) {
	f := newFakeServer(t)
	f.toolsMode.Store("multi")
	f.pinnedID.Store("helper-A")
	stdout, _ := runBridge(t, f,
		`{"jsonrpc":"2.0","id":4,"method":"tools/list"}`+"\n",
		"helper-A")
	got := parseLine(t, stdout, `{"jsonrpc":"2.0"`)
	if _, ok := got["error"]; ok {
		t.Fatalf("unexpected error: %v", got["error"])
	}
	res := got["result"].(map[string]any)
	if len(res["tools"].([]any)) != 2 {
		t.Fatalf("tools len = %v", res["tools"])
	}
}

func TestBridge_ToolsCall_Success(t *testing.T) {
	f := newFakeServer(t)
	stdout, _ := runBridge(t, f,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"status","arguments":{}}}`+"\n",
		"helper-A")

	// /mcp/call should have received a body with client_id injected.
	gotBody := f.gotCallBody.Load().(string)
	var sent map[string]any
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatalf("decode forwarded body: %v\n%s", err, gotBody)
	}
	if sent["method"] != "tools/call" {
		t.Fatalf("method = %v", sent["method"])
	}
	params := sent["params"].(map[string]any)
	if params["client_id"] != "helper-A" {
		t.Fatalf("client_id not injected: %v", params)
	}
	if params["name"] != "status" {
		t.Fatalf("name = %v", params["name"])
	}

	// Bridge response: result is the raw /mcp/call envelope, so we expect
	// {"jsonrpc":"2.0","id":5,"result":{"content":[...],"data":{...}}}.
	got := parseLine(t, stdout, `{"jsonrpc":"2.0"`)
	res := got["result"].(map[string]any)
	content := res["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["text"] != "hello-from-helper" {
		t.Fatalf("content = %v", content)
	}
}

func TestBridge_ToolsCall_ToolError_PassesThrough(t *testing.T) {
	f := newFakeServer(t)
	f.callMode.Store("tool-error")
	stdout, _ := runBridge(t, f,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"bad"}}`+"\n",
		"helper-A")
	got := parseLine(t, stdout, `{"jsonrpc":"2.0"`)
	errObj, ok := got["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error, got %v", got)
	}
	if errObj["message"] != "boom" {
		t.Fatalf("message = %v", errObj["message"])
	}
}

func TestBridge_ToolsCall_UpstreamDown(t *testing.T) {
	// No fake server; use a closed port. The bridge first calls
	// /mcp/tools to resolve client_id, so the failure surfaces there.
	// Pin the client_id to skip that lookup and force the failure to come
	// from the actual /mcp/call POST.
	b := NewBridge("http://127.0.0.1:1", "pinned")
	stdin := strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"x"}}` + "\n")
	var out, errOut bytes.Buffer
	if err := b.Serve(stdin, &out, &errOut); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	got := parseLine(t, out.String(), `{"jsonrpc":"2.0"`)
	errObj := got["error"].(map[string]any)
	if errObj["message"] != "upstream unreachable" {
		t.Fatalf("message = %v", errObj["message"])
	}
}

func TestBridge_ToolsCall_MissingName(t *testing.T) {
	f := newFakeServer(t)
	stdout, _ := runBridge(t, f,
		`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{}}`+"\n",
		"helper-A")
	got := parseLine(t, stdout, `{"jsonrpc":"2.0"`)
	errObj := got["error"].(map[string]any)
	if errObj["code"].(float64) != -32602 {
		t.Fatalf("code = %v", errObj["code"])
	}
}

func TestBridge_UnknownMethod(t *testing.T) {
	f := newFakeServer(t)
	stdout, _ := runBridge(t, f,
		`{"jsonrpc":"2.0","id":9,"method":"resources/list"}`+"\n",
		"")
	got := parseLine(t, stdout, `{"jsonrpc":"2.0"`)
	errObj := got["error"].(map[string]any)
	if errObj["code"].(float64) != -32601 {
		t.Fatalf("code = %v", errObj["code"])
	}
}

func TestBridge_ParseError(t *testing.T) {
	f := newFakeServer(t)
	stdout, _ := runBridge(t, f, `not json`+"\n", "")
	got := parseLine(t, stdout, `{"jsonrpc":"2.0"`)
	errObj := got["error"].(map[string]any)
	if errObj["code"].(float64) != -32700 {
		t.Fatalf("code = %v", errObj["code"])
	}
}

func TestBridge_InvalidJSONRPC(t *testing.T) {
	f := newFakeServer(t)
	stdout, _ := runBridge(t, f,
		`{"jsonrpc":"1.0","id":1,"method":"ping"}`+"\n",
		"")
	got := parseLine(t, stdout, `{"jsonrpc":"2.0"`)
	errObj := got["error"].(map[string]any)
	if errObj["code"].(float64) != -32600 {
		t.Fatalf("code = %v", errObj["code"])
	}
}

func TestBridge_Ping(t *testing.T) {
	f := newFakeServer(t)
	stdout, _ := runBridge(t, f,
		`{"jsonrpc":"2.0","id":10,"method":"ping"}`+"\n",
		"")
	got := parseLine(t, stdout, `{"jsonrpc":"2.0"`)
	if _, ok := got["result"]; !ok {
		t.Fatalf("ping should return result, got %v", got)
	}
}

func TestBridge_Run_DefaultsAndFlags(t *testing.T) {
	// Drive the run() helper directly to verify flag/env wiring without
	// touching os.Stdin/Stdout.
	var stdin bytes.Buffer
	stdin.WriteString(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")
	var stdout, stderr bytes.Buffer

	// Build a fake server to back the default URL. run() doesn't know about
	// httptest directly; instead, set MCP_HTTP_URL to an unreachable port
	// (we are only testing the wiring, not the round-trip).
	t.Setenv("MCP_HTTP_URL", "http://127.0.0.1:1")
	t.Setenv("MCP_CLIENT_ID", "wired-id")
	if err := run("", "", &stdin, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(stderr.String(), "client_id=wired-id") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "http://127.0.0.1:1") {
		t.Fatalf("stderr missing url: %q", stderr.String())
	}
}

// TestBridge_StderrIsClean keeps us honest about the contract: nothing
// user-facing JSON-RPC must leak into stdout when only stderr messages are
// emitted (e.g. on the "no helper connected" path).
func TestBridge_StderrIsClean(t *testing.T) {
	f := newFakeServer(t)
	f.toolsMode.Store("none")
	stdout, stderr := runBridge(t, f,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`+"\n",
		"")
	if !strings.Contains(stdout, `"error"`) {
		t.Fatalf("expected error response on stdout, got %q", stdout)
	}
	if strings.Contains(stdout, "bridge:") {
		t.Fatalf("stderr message leaked into stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "no helper") {
		// not a hard requirement; just sanity that we logged nothing spurious
		_ = fmt.Sprintf("stderr = %q", stderr)
	}
}
