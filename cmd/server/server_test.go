package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tomcat-ops-helper/internal/config"
	"tomcat-ops-helper/internal/protocol"
	"tomcat-ops-helper/internal/wsserver"
)

func TestDecodeCommandBody_JSON(t *testing.T) {
	req := httptest.NewRequest("POST", "/command", strings.NewReader(`{"action":"status","timeout":10}`))
	req.Header.Set("Content-Type", "application/json")
	body, err := decodeCommandBody(req)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Action != "status" {
		t.Fatalf("action = %q", body.Action)
	}
	if body.Timeout != 10 {
		t.Fatalf("timeout = %v", body.Timeout)
	}
}

func TestDecodeCommandBody_Form(t *testing.T) {
	req := httptest.NewRequest("POST", "/command",
		strings.NewReader(`action=status&timeout=15&client_id=c-1`))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	body, err := decodeCommandBody(req)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Action != "status" || body.Timeout != 15 || body.ClientID != "c-1" {
		t.Fatalf("unexpected body: %+v", body)
	}
}

func TestDecodeCommandBody_FormWithCharset(t *testing.T) {
	req := httptest.NewRequest("POST", "/command",
		strings.NewReader(`action=exec`))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	body, err := decodeCommandBody(req)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Action != "exec" {
		t.Fatalf("action = %q", body.Action)
	}
}

func TestDecodeCommandBody_RejectsBadContentType(t *testing.T) {
	req := httptest.NewRequest("POST", "/command", strings.NewReader("garbage"))
	req.Header.Set("Content-Type", "text/plain")
	if _, err := decodeCommandBody(req); err == nil {
		t.Fatal("expected error for unsupported content-type")
	}
}

func TestDecodeCommandBody_SniffsJSONWithoutContentType(t *testing.T) {
	// Curl's default Content-Type is application/x-www-form-urlencoded, but
	// users frequently paste JSON bodies anyway. The decoder should accept
	// them.
	req := httptest.NewRequest("POST", "/command",
		strings.NewReader(`{"action":"status","timeout":7}`))
	// No Content-Type set, mimicking `curl -d '...'`.
	body, err := decodeCommandBody(req)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Action != "status" || body.Timeout != 7 {
		t.Fatalf("unexpected: %+v", body)
	}
}

func TestDecodeCommandBody_SniffsJSONDespiteFormContentType(t *testing.T) {
	req := httptest.NewRequest("POST", "/command",
		strings.NewReader(`{"action":"exec"}`))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	body, err := decodeCommandBody(req)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Action != "exec" {
		t.Fatalf("action = %q", body.Action)
	}
}

func newTestRelay(t *testing.T) *relay {
	t.Helper()
	cfg := &config.ServerConfig{
		Listen:  ":0",
		Tokens:  []string{"tok-1234567890"},
		TLSCert: "/c", TLSKey: "/k",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg: %v", err)
	}
	return newRelay(cfg, testLogger(), func(map[string]any) {})
}

func testLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

func TestStoreAndServeTools(t *testing.T) {
	r := newTestRelay(t)
	client := &wsserver.Client{ID: "c-1"}
	tl := protocol.ToolList{Tools: []protocol.ToolMeta{
		{Name: "status", Description: "status", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "tail", Description: "tail", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}}
	r.storeTools(client, tl)

	req := httptest.NewRequest(http.MethodGet, "/mcp/tools?client_id=c-1", nil)
	rec := httptest.NewRecorder()
	r.handleMcpTools(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		ClientID string               `json:"client_id"`
		Tools    []protocol.ToolMeta `json:"tools"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ClientID != "c-1" || len(got.Tools) != 2 {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestHandleMcpTools_AggregatedView(t *testing.T) {
	r := newTestRelay(t)
	r.storeTools(&wsserver.Client{ID: "a"}, protocol.ToolList{Tools: []protocol.ToolMeta{{Name: "x"}}})
	r.storeTools(&wsserver.Client{ID: "b"}, protocol.ToolList{Tools: []protocol.ToolMeta{{Name: "y"}}})

	req := httptest.NewRequest(http.MethodGet, "/mcp/tools", nil)
	rec := httptest.NewRecorder()
	r.handleMcpTools(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"client_id":"a"`) ||
		!strings.Contains(rec.Body.String(), `"client_id":"b"`) {
		t.Fatalf("aggregated missing entries: %s", rec.Body.String())
	}
}

func TestHandleMcpTools_UnknownClient(t *testing.T) {
	r := newTestRelay(t)
	req := httptest.NewRequest(http.MethodGet, "/mcp/tools?client_id=nope", nil)
	rec := httptest.NewRecorder()
	r.handleMcpTools(rec, req)
	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestUnregisterClearsTools(t *testing.T) {
	r := newTestRelay(t)
	c := &wsserver.Client{ID: "c-2"}
	r.storeTools(c, protocol.ToolList{Tools: []protocol.ToolMeta{{Name: "x"}}})
	r.unregister(c)
	req := httptest.NewRequest(http.MethodGet, "/mcp/tools?client_id=c-2", nil)
	rec := httptest.NewRecorder()
	r.handleMcpTools(rec, req)
	if rec.Code != 404 {
		t.Fatalf("expected 404 after unregister, got %d", rec.Code)
	}
}

func TestHandleMcpCall_RejectsUnknownTool(t *testing.T) {
	r := newTestRelay(t)
	r.clients["c-1"] = &wsserver.Client{ID: "c-1"}
	r.tools["c-1"] = []protocol.ToolMeta{{Name: "status"}}

	body := `{"jsonrpc":"2.0","id":"1","method":"tools/call","params":{"name":"no_such","client_id":"c-1"}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp/call", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.handleMcpCall(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var resp jsonrpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Fatalf("expected -32602 tool not advertised, got %+v", resp.Error)
	}
}

func TestHandleMcpCall_RejectsMissingClientID(t *testing.T) {
	r := newTestRelay(t)
	r.clients["c-1"] = &wsserver.Client{ID: "c-1"}
	r.clients["c-2"] = &wsserver.Client{ID: "c-2"}
	r.tools["c-1"] = []protocol.ToolMeta{{Name: "status"}}

	body := `{"jsonrpc":"2.0","id":"1","method":"tools/call","params":{"name":"status"}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp/call", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.handleMcpCall(rec, req)
	var resp jsonrpcResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error == nil || resp.Error.Code != -32004 {
		t.Fatalf("expected -32004 client_id required, got %+v", resp.Error)
	}
}

func TestHandleMcpCall_RejectsBadJSONRPC(t *testing.T) {
	r := newTestRelay(t)
	body := `{"jsonrpc":"1.0","id":"1","method":"tools/call","params":{}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp/call", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.handleMcpCall(rec, req)
	var resp jsonrpcResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Error == nil {
		t.Fatalf("expected error for jsonrpc != 2.0, got %+v", resp)
	}
}

// TestUnregister_FailsPendingCallsOfThatClient ensures a helper disconnect
// resolves that helper's in-flight calls immediately with a synthetic
// error instead of parking the HTTP caller for the full (now 300s) default
// call timeout (2026-09-25 report follow-up).
func TestUnregister_FailsPendingCallsOfThatClient(t *testing.T) {
	r := newTestRelay(t)
	mine := make(chan protocol.Result, 1)
	other := make(chan protocol.Result, 1)
	r.mu.Lock()
	r.pending["cmd-1"] = pendingCall{ch: mine, clientID: "c-1"}
	r.pending["cmd-2"] = pendingCall{ch: other, clientID: "c-2"}
	r.mu.Unlock()

	r.unregister(&wsserver.Client{ID: "c-1"})

	select {
	case res := <-mine:
		if res.Success {
			t.Fatal("synthetic result must not be success")
		}
		if res.Error != "helper disconnected before result" {
			t.Fatalf("unexpected synthetic error: %q", res.Error)
		}
	default:
		t.Fatal("expected pending call of the disconnected helper to be resolved")
	}
	select {
	case <-other:
		t.Fatal("pending call of another helper must be left alone")
	default:
	}
	r.mu.Lock()
	_, ok1 := r.pending["cmd-1"]
	_, ok2 := r.pending["cmd-2"]
	r.mu.Unlock()
	if ok1 {
		t.Fatal("cmd-1 should have been removed from pending")
	}
	if !ok2 {
		t.Fatal("cmd-2 should still be pending")
	}
}
