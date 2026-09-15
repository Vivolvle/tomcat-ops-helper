package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tomcat-ops-helper/internal/protocol"
	"tomcat-ops-helper/internal/wsserver"
)

func toolNames(tools []protocol.ToolMeta) []string {
	names := make([]string, len(tools))
	for i, tl := range tools {
		names[i] = tl.Name
	}
	return names
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestHandleMcpTools_AggregatedExposesServerTools(t *testing.T) {
	r := newTestRelay(t)
	r.storeTools(&wsserver.Client{ID: "a"}, protocol.ToolList{Tools: []protocol.ToolMeta{{Name: "status"}}})

	req := httptest.NewRequest(http.MethodGet, "/mcp/tools", nil)
	rec := httptest.NewRecorder()
	r.handleMcpTools(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var got struct {
		ServerTools []protocol.ToolMeta `json:"server_tools"`
		Clients     []struct {
			ClientID string              `json:"client_id"`
			Tools    []protocol.ToolMeta `json:"tools"`
		} `json:"clients"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !containsString(toolNames(got.ServerTools), "report_problem") {
		t.Fatalf("server_tools missing report_problem: %s", rec.Body.String())
	}
	if len(got.Clients) != 1 || len(got.Clients[0].Tools) != 1 || got.Clients[0].Tools[0].Name != "status" {
		t.Fatalf("helper tools should stay helper-only: %+v", got.Clients)
	}
}

func TestHandleMcpTools_PinnedExposesServerTools(t *testing.T) {
	r := newTestRelay(t)
	r.storeTools(&wsserver.Client{ID: "c-1"}, protocol.ToolList{Tools: []protocol.ToolMeta{{Name: "status"}}})

	req := httptest.NewRequest(http.MethodGet, "/mcp/tools?client_id=c-1", nil)
	rec := httptest.NewRecorder()
	r.handleMcpTools(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var got struct {
		ServerTools []protocol.ToolMeta `json:"server_tools"`
		Tools       []protocol.ToolMeta `json:"tools"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "status" {
		t.Fatalf("pinned helper tools = %+v", got.Tools)
	}
	if !containsString(toolNames(got.ServerTools), "report_problem") {
		t.Fatalf("server_tools missing report_problem: %s", rec.Body.String())
	}
}

type mcpCallResult struct {
	Result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Data struct {
			OK    bool   `json:"ok"`
			Path  string `json:"path"`
			Bytes int    `json:"bytes"`
		} `json:"data"`
	} `json:"result"`
	Error *jsonrpcError `json:"error"`
}

func callMcpRaw(t *testing.T, r *relay, body string) mcpCallResult {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp/call", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.handleMcpCall(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("http status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var got mcpCallResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	return got
}

func TestReportProblem_AppendsEntryWithoutHelper(t *testing.T) {
	r := newTestRelay(t)
	logPath := filepath.Join(t.TempDir(), "problem.md")
	r.cfg.ProblemLog = logPath

	body := `{"jsonrpc":"2.0","id":"1","method":"tools/call","params":{"name":"report_problem",
		"arguments":{"description":"exec eats quotes","tool":"exec","expected":"argv preserved","source":"proj-x"}}}`
	got := callMcpRaw(t, r, body)
	if got.Error != nil {
		t.Fatalf("unexpected error: %+v", got.Error)
	}
	if !got.Result.Data.OK || got.Result.Data.Path != logPath {
		t.Fatalf("unexpected data: %+v", got.Result.Data)
	}
	if got.Result.Data.Bytes <= 0 {
		t.Fatalf("bytes = %d", got.Result.Data.Bytes)
	}
	if len(got.Result.Content) != 1 || !strings.Contains(got.Result.Content[0].Text, logPath) {
		t.Fatalf("content = %+v", got.Result.Content)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read problem log: %v", err)
	}
	text := string(raw)
	for _, want := range []string{
		"# problem.md",
		"- 问题: exec eats quotes",
		"- 工具: exec",
		"- 期望: argv preserved",
		"- 来源: proj-x",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("problem.md missing %q:\n%s", want, text)
		}
	}
	if strings.Count(text, "# problem.md") != 1 {
		t.Fatalf("header should appear exactly once:\n%s", text)
	}
}

func TestReportProblem_SecondCallAppendsNoDuplicateHeader(t *testing.T) {
	r := newTestRelay(t)
	logPath := filepath.Join(t.TempDir(), "problem.md")
	r.cfg.ProblemLog = logPath

	call := func(desc string) {
		body := `{"jsonrpc":"2.0","id":"1","method":"tools/call","params":{"name":"report_problem","arguments":{"description":"` + desc + `"}}}`
		if got := callMcpRaw(t, r, body); got.Error != nil {
			t.Fatalf("unexpected error: %+v", got.Error)
		}
	}
	call("first issue")
	call("second issue")

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read problem log: %v", err)
	}
	text := string(raw)
	if strings.Count(text, "# problem.md") != 1 {
		t.Fatalf("header should appear exactly once:\n%s", text)
	}
	if strings.Count(text, "## [") != 2 {
		t.Fatalf("expected 2 entries:\n%s", text)
	}
	if !strings.Contains(text, "first issue") || !strings.Contains(text, "second issue") {
		t.Fatalf("entries missing:\n%s", text)
	}
}

func TestReportProblem_RequiresDescription(t *testing.T) {
	r := newTestRelay(t)
	r.cfg.ProblemLog = filepath.Join(t.TempDir(), "problem.md")
	body := `{"jsonrpc":"2.0","id":"1","method":"tools/call","params":{"name":"report_problem","arguments":{"tool":"exec"}}}`
	got := callMcpRaw(t, r, body)
	if got.Error == nil || got.Error.Code != -32602 {
		t.Fatalf("expected -32602, got %+v", got.Error)
	}
	if _, err := os.Stat(r.cfg.ProblemLog); !os.IsNotExist(err) {
		t.Fatalf("problem log should not be created on validation failure")
	}
}

func TestHandleMcpCall_NoHelperConnected(t *testing.T) {
	r := newTestRelay(t)
	body := `{"jsonrpc":"2.0","id":"1","method":"tools/call","params":{"name":"status"}}`
	got := callMcpRaw(t, r, body)
	if got.Error == nil || got.Error.Code != -32004 {
		t.Fatalf("expected -32004, got %+v", got.Error)
	}
	if !strings.Contains(got.Error.Message, "no helper") {
		t.Fatalf("message = %q", got.Error.Message)
	}
}
