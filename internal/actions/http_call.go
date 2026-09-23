package actions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var httpCallTool = Tool{
	Name:        "http_call",
	Description: "Call an explicit HTTP(S) URL from the helper machine. Pass request headers directly in the headers object, including Authorization, X-Access-Token, Cookie and X-Sign; values are sent unchanged. Credential files are NOT required. No application header-name allowlist or header-count limit is imposed; names must be HTTP tokens and values must be strings without CR/LF/control characters. Quote numeric X-TIMESTAMP and X-Tenant-Id values. debug=true returns sanitized request metadata; dry_run=true validates and assembles without sending any request (not a connectivity test). Body accepts a raw string or JSON object. headers_file and token_file are optional alternatives only when the caller chooses file-based credentials; those files must be in allowed_dirs. Redirects and automatic retries are disabled. HTTP error statuses return ok=false with their response body. Known credential values are redacted from returned headers/body. Use an explicit port (e.g. 8060 prod, 8080 dev); no environment guessing. For a real local HTTP smoke test without touching business APIs use url=helper://echo; it starts a temporary loopback echo listener and closes it after the call.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"debug":{"type":"boolean","default":false},"dry_run":{"type":"boolean","default":false},"url":{"type":"string"},"method":{"type":"string","default":"GET"},"headers":{"type":"object","description":"Request headers sent as supplied. Tokens can be passed directly in Authorization or X-Access-Token; no credential file is required.","additionalProperties":{"type":"string"}},"body":{"type":["string","object"]},"timeout":{"type":"integer","minimum":1,"maximum":300,"default":30},"max_response_bytes":{"type":"integer","minimum":1,"maximum":1048576,"default":65536},"headers_file":{"type":"string"},"token_file":{"type":"string"},"token_field":{"type":"string","default":"token","description":"Dot-separated JSON field path, e.g. data.access_token"},"token_header":{"type":"string","default":"Authorization"},"token_prefix":{"type":"string","default":"Bearer "}},"required":["url"],"additionalProperties":false}`),
}

// Credential files never leave this handler: neither their content nor parse
// errors (which can contain input fragments) are sent through the relay.
func (r *Registry) httpCredentialFile(path string) ([]byte, error) {
	abs, err := filepath.Abs(path)
	if err != nil || !r.cfg.IsAllowedWorkDir(abs) {
		return nil, fmt.Errorf("http_call: credential file outside allowed_dirs")
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil || !r.cfg.IsAllowedWorkDir(abs) {
		return nil, fmt.Errorf("http_call: credential file unavailable or resolved outside allowed_dirs")
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, fmt.Errorf("http_call: cannot open credential file")
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("http_call: credential file must be regular")
	}
	b, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(b) > 65536 {
		return nil, fmt.Errorf("http_call: cannot read credential file (64 KiB maximum)")
	}
	return bytes.TrimPrefix(b, []byte{0xef, 0xbb, 0xbf}), nil
}

func (r *Registry) httpCall(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	p := struct {
		URL         string            `json:"url"`
		Method      string            `json:"method"`
		Headers     map[string]string `json:"headers"`
		Body        json.RawMessage   `json:"body"`
		Timeout     int               `json:"timeout"`
		MaxBytes    int               `json:"max_response_bytes"`
		HeadersFile string            `json:"headers_file"`
		TokenFile   string            `json:"token_file"`
		TokenField  string            `json:"token_field"`
		TokenHeader string            `json:"token_header"`
		TokenPrefix string            `json:"token_prefix"`
		Debug       bool              `json:"debug"`
		DryRun      bool              `json:"dry_run"`
	}{Method: "GET", Timeout: 30, MaxBytes: 65536, TokenField: "token", TokenHeader: "Authorization", TokenPrefix: "Bearer "}
	if err := validateHTTPParams(raw); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return nil, fmt.Errorf("http_call: %s must be %s", typeErr.Field, typeErr.Type)
		}
		return nil, fmt.Errorf("http_call: parameters could not be decoded")
	}
	if p.Timeout < 1 || p.Timeout > 300 || p.MaxBytes < 1 || p.MaxBytes > textFileLimit {
		return nil, fmt.Errorf("http_call: timeout must be 1..300 seconds; max_response_bytes must be 1..1048576")
	}
	// A temporary loopback-only echo server exercises actual HTTP transport
	// without contacting a business API or leaving a permanent listener behind.
	if p.URL == "helper://echo" {
		if p.DryRun {
			p.URL = "http://127.0.0.1/__echo__"
		} else {
			echoURL, closeEcho, err := startHTTPEcho()
			if err != nil {
				return nil, fmt.Errorf("http_call: cannot start loopback echo listener")
			}
			defer closeEcho()
			p.URL = echoURL
		}
	}
	u, err := url.Parse(p.URL)
	if err != nil {
		return nil, fmt.Errorf("http_call: url has invalid syntax, escaping, or port")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("http_call: url scheme must be http or https")
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("http_call: url hostname is required")
	}
	if u.User != nil {
		return nil, fmt.Errorf("http_call: url userinfo is unsupported; use headers for authentication")
	}
	if u.Fragment != "" {
		return nil, fmt.Errorf("http_call: url fragment is unsupported")
	}
	if !validHTTPToken(p.Method) {
		return nil, fmt.Errorf("http_call: method must be a nonempty HTTP token without spaces or control characters")
	}
	var body []byte
	jsonBody := false
	if len(p.Body) > 0 {
		if p.Body[0] == '"' {
			var s string
			if json.Unmarshal(p.Body, &s) != nil {
				return nil, fmt.Errorf("http_call: invalid body")
			}
			body = []byte(s)
		} else if p.Body[0] == '{' {
			body = p.Body
			jsonBody = true
		} else {
			return nil, fmt.Errorf("http_call: body must be a string or object")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.Timeout)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, strings.ToUpper(p.Method), p.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("http_call: invalid method or URL")
	}
	// Prevent net/http from replaying a write on a stale pooled connection.
	req.GetBody = nil
	secrets := []string{}
	for k, v := range p.Headers {
		req.Header.Set(k, v)
		if sensitiveHTTPHeader(k) {
			secrets = append(secrets, v)
		}
	}
	if jsonBody && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if p.HeadersFile != "" {
		b, err := r.httpCredentialFile(p.HeadersFile)
		if err != nil {
			return nil, err
		}
		var headers map[string]string
		if json.Unmarshal(b, &headers) != nil || len(headers) == 0 {
			return nil, fmt.Errorf("http_call: headers_file must contain a nonempty JSON header dictionary")
		}
		for k, v := range headers {
			req.Header.Set(k, v)
			secrets = append(secrets, v)
		}
	}
	if p.TokenFile != "" {
		b, err := r.httpCredentialFile(p.TokenFile)
		if err != nil {
			return nil, err
		}
		var value any
		if json.Unmarshal(b, &value) != nil {
			return nil, fmt.Errorf("http_call: invalid token JSON")
		}
		for _, key := range strings.Split(p.TokenField, ".") {
			obj, ok := value.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("http_call: token_field not found")
			}
			value = obj[key]
		}
		token, ok := value.(string)
		if !ok || token == "" || p.TokenHeader == "" {
			return nil, fmt.Errorf("http_call: nonempty string token and token_header required")
		}
		req.Header.Set(p.TokenHeader, p.TokenPrefix+token)
		secrets = append(secrets, p.TokenPrefix+token, token)
	}
	for k, values := range req.Header {
		if !validHTTPToken(k) {
			return nil, fmt.Errorf("http_call: invalid header name %q: use HTTP token characters (letters, digits, hyphens); no spaces or controls", k)
		}
		for _, v := range values {
			for i := 0; i < len(v); i++ {
				if (v[i] < 32 && v[i] != '\t') || v[i] == 127 {
					return nil, fmt.Errorf("http_call: headers.%s contains a forbidden control character (including CR/LF)", k)
				}
			}
		}
	}
	// Also hide bearer values independently of their scheme, and cookie values
	// independently of their names when an endpoint echoes request credentials.
	for _, s := range append([]string(nil), secrets...) {
		if strings.HasPrefix(strings.ToLower(s), "bearer ") {
			secrets = append(secrets, s[7:])
		}
		for _, cookie := range strings.Split(s, ";") {
			if _, v, ok := strings.Cut(strings.TrimSpace(cookie), "="); ok {
				secrets = append(secrets, v)
			}
		}
	}
	for _, secret := range append([]string(nil), secrets...) {
		encoded, _ := json.Marshal(secret)
		secrets = append(secrets, string(encoded[1:len(encoded)-1]), url.QueryEscape(secret))
	}
	redact := func(s string) string {
		for _, secret := range secrets {
			if secret == "" {
				continue
			}
			s = strings.ReplaceAll(s, secret, "[REDACTED]")
			encoded, _ := json.Marshal(secret)
			s = strings.ReplaceAll(s, string(encoded[1:len(encoded)-1]), "[REDACTED]")
			s = strings.ReplaceAll(s, url.QueryEscape(secret), "[REDACTED]")
		}
		return s
	}
	// Request diagnostics intentionally omit body and header values and all
	// query values, so even application-specific credentials remain private.
	diagnosticHeaders := map[string]string{}
	for k := range req.Header {
		diagnosticHeaders[k] = "[REDACTED]"
	}
	safeURL := *u
	query := safeURL.Query()
	for k := range query {
		query[k] = []string{"[REDACTED]"}
	}
	safeURL.RawQuery = query.Encode()
	diagnostic := map[string]any{"method": req.Method, "url": redact(safeURL.String()), "headers": diagnosticHeaders, "body_bytes": len(body), "timeout_seconds": p.Timeout, "follow_redirects": false}
	if p.DryRun {
		return mustMarshal(map[string]any{"ok": true, "dry_run": true, "sent": false, "debug": diagnostic}), nil
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableKeepAlives = true
	transport.MaxResponseHeaderBytes = 65536
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		var debugData json.RawMessage
		if p.Debug {
			debugData = mustMarshal(map[string]any{"ok": false, "debug": diagnostic})
		}
		if ctx.Err() != nil {
			return debugData, fmt.Errorf("http_call: request canceled or timed out; a write may already have taken effect; do not automatically retry")
		}
		return debugData, fmt.Errorf("http_call: request failed (network or TLS); a write may already have taken effect; do not automatically retry")
	}
	defer resp.Body.Close()
	b, readErr := io.ReadAll(io.LimitReader(resp.Body, int64(p.MaxBytes)+1))
	// Redact before truncation to avoid returning a partial credential.
	text := redact(string(b))
	truncated := len(b) > p.MaxBytes
	if truncated || readErr != nil {
		// A credential may straddle the read limit. Remove matching trailing
		// prefixes before applying the display cap.
		for _, secret := range secrets {
			maxPrefix := len(secret) - 1
			if maxPrefix > len(text) {
				maxPrefix = len(text)
			}
			for n := maxPrefix; n > 0; n-- {
				if strings.HasSuffix(text, secret[:n]) {
					text = text[:len(text)-n]
					break
				}
			}
		}
		if len(text) > p.MaxBytes {
			text = text[:p.MaxBytes]
		}
	}
	headers := map[string][]string{}
	for k, values := range resp.Header {
		if sensitiveHTTPHeader(k) {
			headers[k] = []string{"[REDACTED]"}
			continue
		}
		for _, v := range values {
			headers[k] = append(headers[k], redact(v))
		}
	}
	result := map[string]any{"status_code": resp.StatusCode, "ok": resp.StatusCode >= 200 && resp.StatusCode < 300 && readErr == nil, "headers": headers, "body": text, "truncated": truncated, "took_ms": time.Since(start).Milliseconds()}
	if p.Debug {
		result["debug"] = diagnostic
	}
	if readErr != nil {
		result["read_error"] = "response body incomplete; do not automatically retry writes"
	}
	return mustMarshal(result), nil
}

func validHTTPToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)) {
			continue
		}
		return false
	}
	return true
}

func startHTTPEcho() (string, func(), error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, MaxHeaderBytes: 65536}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(io.LimitReader(req.Body, textFileLimit+1))
		if err != nil || len(body) > textFileLimit {
			http.Error(w, "echo body exceeds 1 MiB or could not be read", http.StatusRequestEntityTooLarge)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"echo": true, "method": req.Method, "path": req.URL.Path, "headers": req.Header, "body": string(body)})
	})
	go func() { _ = server.Serve(listener) }()
	return "http://" + listener.Addr().String() + "/__echo__", func() { _ = server.Close() }, nil
}

// Validate each field separately so errors identify the offending field
// without printing its value (which may be a credential).
func validateHTTPParams(raw json.RawMessage) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) {
			return fmt.Errorf("http_call: malformed parameter JSON at byte %d", syntax.Offset)
		}
		return fmt.Errorf("http_call: parameters must be a JSON object")
	}
	if fields == nil {
		return fmt.Errorf("http_call: parameters must be a JSON object")
	}
	for _, k := range sortedKeysRawHTTP(fields) {
		v := fields[k]
		kind := ""
		switch k {
		case "url", "method", "headers_file", "token_file", "token_field", "token_header", "token_prefix":
			kind = "string"
		case "timeout", "max_response_bytes":
			kind = "integer"
		case "debug", "dry_run":
			kind = "boolean"
		case "body":
			if v[0] != '"' && v[0] != '{' {
				return fmt.Errorf("http_call: body must be a string or object (empty string/object allowed)")
			}
			continue
		case "headers":
			var headers map[string]json.RawMessage
			if v[0] != '{' || json.Unmarshal(v, &headers) != nil {
				return fmt.Errorf("http_call: headers must be an object of string values")
			}
			for _, h := range sortedKeysRawHTTP(headers) {
				if headers[h][0] != '"' {
					return fmt.Errorf("http_call: headers.%s must be a string; quote numeric timestamps and tenant IDs", h)
				}
			}
			continue
		default:
			return fmt.Errorf("http_call: unknown parameter %q; see tool inputSchema", k)
		}
		valid := false
		switch kind {
		case "string":
			valid = v[0] == '"'
		case "integer":
			var n int
			valid = string(v) != "null" && json.Unmarshal(v, &n) == nil
		case "boolean":
			valid = string(v) == "true" || string(v) == "false"
		}
		if !valid {
			return fmt.Errorf("http_call: %s must be a %s", k, kind)
		}
	}
	return nil
}

func sortedKeysRawHTTP(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sensitiveHTTPHeader(k string) bool {
	k = strings.ToLower(k)
	return strings.Contains(k, "authorization") || strings.Contains(k, "cookie") || strings.Contains(k, "token") || strings.Contains(k, "api-key") || strings.Contains(k, "apikey") || k == "x-sign"
}
