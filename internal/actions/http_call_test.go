package actions

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHTTPCallInlineCredentialsWithoutFiles(t *testing.T) {
	headers := map[string]string{
		"Authorization":  "test-jwt-unchanged",
		"X-Access-Token": "test-jwt-unchanged",
		"Cookie":         "session=test-session",
		"X-Sign":         "test-signature",
		"X-TIMESTAMP":    "1790144860313",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "POST" || req.URL.Query().Get("eventId") != "2" {
			t.Error("method or query changed")
		}
		for k, want := range headers {
			if req.Header.Get(k) != want {
				t.Errorf("header %s not sent unchanged", k)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(headers)
	}))
	defer srv.Close()
	// No allowed directories or credential files are needed for inline headers.
	cfg := mustCfg(t)
	cfg.AllowedDirs = nil
	data, err := NewRegistry(cfg, nil).Dispatch(context.Background(), "http_call", mustMarshal(map[string]any{
		"url": srv.URL + "/server/esb/event/retryOne?eventId=2", "method": "POST", "headers": headers, "body": map[string]any{},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"ok":true`) {
		t.Fatalf("request failed: %s", data)
	}
	for _, secret := range []string{"test-jwt-unchanged", "test-session", "test-signature"} {
		if strings.Contains(string(data), secret) {
			t.Fatal("credential echoed in response")
		}
	}
}

func TestHTTPCallCredentialsAndBody(t *testing.T) {
	cfg := mustCfg(t)
	tokenFile := filepath.Join(cfg.TomcatHome, "ai-dev-token.json")
	if err := os.WriteFile(tokenFile, []byte(`{"data":{"access_token":"secret-123"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "POST" || req.Header.Get("Authorization") != "Bearer secret-123" || req.Header.Get("Content-Type") != "application/json" {
			t.Errorf("incorrect request metadata")
		}
		var body map[string]string
		if json.NewDecoder(req.Body).Decode(&body) != nil || body["yaml"] != "中文\nroute: test" {
			t.Errorf("incorrect body")
		}
		w.Header().Set("Set-Cookie", "session=new-secret")
		w.Header().Set("X-Echo", "secret-123")
		w.WriteHeader(422)
		_, _ = w.Write([]byte(`{"echo":"Bearer secret-123","error":"invalid route"}`))
	}))
	defer srv.Close()
	r := NewRegistry(cfg, nil)
	data, err := r.Dispatch(context.Background(), "http_call", mustMarshal(map[string]any{"url": srv.URL, "method": "POST", "body": map[string]string{"yaml": "中文\nroute: test"}, "token_file": tokenFile, "token_field": "data.access_token"}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret") {
		t.Fatalf("credential leaked: %s", data)
	}
	var result struct {
		Status int    `json:"status_code"`
		OK     bool   `json:"ok"`
		Body   string `json:"body"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != 422 || result.OK || !strings.Contains(result.Body, "invalid route") {
		t.Fatalf("bad result: %s", data)
	}
}

func TestHTTPCallHeadersFileRawBodyAndRedirect(t *testing.T) {
	cfg := mustCfg(t)
	path := filepath.Join(cfg.TomcatHome, "headers.json")
	if err := os.WriteFile(path, []byte(`{"X-Custom-Auth":"private-value"}`), 0600); err != nil {
		t.Fatal(err)
	}
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected = true }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Custom-Auth") != "private-value" {
			t.Error("missing file header")
		}
		buf := make([]byte, 20)
		n, _ := r.Body.Read(buf)
		if string(buf[:n]) != "a=中文" {
			t.Error("raw body changed")
		}
		w.Header().Set("Location", target.URL)
		w.WriteHeader(307)
	}))
	defer srv.Close()
	data, err := NewRegistry(cfg, nil).httpCall(context.Background(), mustMarshal(map[string]any{"url": srv.URL, "method": "POST", "body": "a=中文", "headers_file": path}))
	if err != nil || redirected || !strings.Contains(string(data), `"status_code":307`) {
		t.Fatalf("redirect policy: %s %v", data, err)
	}
}

func TestHTTPCallLimitsAndCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("abcdefghij")) }))
	defer srv.Close()
	r := NewRegistry(mustCfg(t), nil)
	data, err := r.httpCall(context.Background(), mustMarshal(map[string]any{"url": srv.URL, "max_response_bytes": 4}))
	if err != nil || !strings.Contains(string(data), `"body":"abcd"`) || !strings.Contains(string(data), `"truncated":true`) {
		t.Fatalf("limit: %s %v", data, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.httpCall(ctx, mustMarshal(map[string]any{"url": srv.URL})); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestHTTPCallValidation(t *testing.T) {
	r := NewRegistry(mustCfg(t), nil)
	for _, p := range []map[string]any{
		{"url": "file:///etc/passwd"}, {"url": "http://user:pass@localhost/"},
		{"url": "http://localhost/", "timeout": 0}, {"url": "http://localhost/", "max_response_bytes": 1048577},
		{"url": "http://localhost/", "body": 42},
		{"url": "http://localhost/", "token_file": filepath.Join(t.TempDir(), "token.json")},
	} {
		if _, err := r.httpCall(context.Background(), mustMarshal(p)); err == nil {
			t.Errorf("accepted invalid parameters: %v", p)
		}
	}
	found := false
	for _, tool := range r.Tools() {
		if tool.Name == "http_call" {
			found = true
			if !json.Valid(tool.InputSchema) {
				t.Fatal("invalid schema")
			}
		}
	}
	if !found {
		t.Fatal("http_call not advertised")
	}
}

func TestHTTPCallTruncatedCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("prefix secret-123 remainder")) }))
	defer srv.Close()
	data, err := NewRegistry(mustCfg(t), nil).httpCall(context.Background(), mustMarshal(map[string]any{"url": srv.URL, "headers": map[string]string{"Authorization": "Bearer secret-123"}, "max_response_bytes": 10}))
	if err != nil || strings.Contains(string(data), "sec") {
		t.Fatalf("partial credential leak: %s %v", data, err)
	}
}

func TestHTTPCallFieldErrors(t *testing.T) {
	r := NewRegistry(mustCfg(t), nil)
	for _, tc := range []struct{ raw, want string }{
		{`{"url":"http://localhost","headers":{"X-TIMESTAMP":1790144860313}}`, "headers.X-TIMESTAMP must be a string"},
		{`{"url":"http://localhost","headers":{"X-Tenant-Id":1000}}`, "headers.X-Tenant-Id must be a string"},
		{`{"url":"http://localhost","headers":{"Authorization":null}}`, "headers.Authorization must be a string"},
		{`{"url":"http://localhost","headers":{"X-Sign":"secret\r\nvalue"}}`, "headers.X-Sign contains"},
		{`{"url":"http://localhost","headers":{"bad name":"secret"}}`, "invalid header name"},
		{`{"url":"http://localhost","body":[]}`, "body must be a string or object"},
		{`{"url":"http://localhost","timeout":"30"}`, "timeout must be a integer"},
		{`{"url":"http://localhost","verbose":true}`, "unknown parameter"},
		{`{"url":"http://localhost","debug":null}`, "debug must be a boolean"},
		{`{"url":"http://localhost"} {}`, "malformed parameter JSON"},
		{`null`, "parameters must be a JSON object"},
	} {
		_, err := r.httpCall(context.Background(), json.RawMessage(tc.raw))
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "secret") {
			t.Errorf("want %s; got %v", tc.want, err)
		}
	}
}

func TestHTTPCallDryRunAndEcho(t *testing.T) {
	r := NewRegistry(mustCfg(t), nil)
	params := map[string]any{"url": "http://127.0.0.1:1/retry?token=query-secret", "method": "POST", "body": map[string]any{}, "debug": true, "dry_run": true, "headers": map[string]string{
		"Content-Type": "application/json;charset=UTF-8", "X-Access-Token": "jwt-secret", "Authorization": "jwt-secret", "X-Sign": "sign-secret", "X-TIMESTAMP": "1790144860313", "X-Tenant-Id": "1000", "X-Version": "v3", "Accept": "application/json, text/plain, */*",
	}}
	data, err := r.httpCall(context.Background(), mustMarshal(params))
	if err != nil || !strings.Contains(string(data), `"sent":false`) || strings.Contains(string(data), "secret") {
		t.Fatalf("dry run: %s %v", data, err)
	}
	params["url"] = "helper://echo"
	params["dry_run"] = false
	data, err = r.httpCall(context.Background(), mustMarshal(params))
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Status int            `json:"status_code"`
		Body   string         `json:"body"`
		Debug  map[string]any `json:"debug"`
	}
	if json.Unmarshal(data, &result) != nil || result.Status != 200 || result.Debug == nil {
		t.Fatalf("echo failed: %s", data)
	}
	var echoed struct {
		Method  string      `json:"method"`
		Headers http.Header `json:"headers"`
		Body    string      `json:"body"`
	}
	if json.Unmarshal([]byte(result.Body), &echoed) != nil || echoed.Method != "POST" || echoed.Body != "{}" || echoed.Headers.Get("X-TIMESTAMP") != "1790144860313" || echoed.Headers.Get("Authorization") != "[REDACTED]" {
		t.Fatalf("echo mismatch: %s", data)
	}
	if strings.Contains(string(data), "secret") {
		t.Fatal("echo leaked credentials")
	}
}
