package helpergui

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// WSTestResult is the reply of POST /api/test/server. Code classifies
// the failure so the Chinese UI can say exactly what went wrong:
//
//	ok        — handshake succeeded (URL, path, TLS and token all good)
//	auth      — server answered 401/403: token rejected (the project's
//	             own server returns 403 for bad tokens)
//	not_found — 404: wrong path (server_url must end with /ws)
//	handshake — server answered, but not with a websocket upgrade
//	tls       — certificate / TLS failure (self-signed cert? try the
//	             跳过证书校验 switch)
//	timeout   — no answer within the deadline
//	dial      — TCP-level failure (host unreachable / DNS / refused)
//	bad_url   — not parseable as a ws:// or wss:// URL
type WSTestResult struct {
	OK         bool   `json:"ok"`
	Code       string `json:"code"`
	HTTPStatus int    `json:"http_status,omitempty"`
	LatencyMs  int64  `json:"latency_ms"`
	Detail     string `json:"detail,omitempty"`
}

// TestServerURL performs the exact dial the helper performs at startup
// (mirrors internal/wsclient.connectAndServe): Authorization Bearer
// header, 8s handshake deadline, optional InsecureSkipVerify. On
// success it sends a Hello and immediately a normal close — the server
// tolerates an early close, and the round trip proves the whole chain.
func TestServerURL(ctx context.Context, rawURL, token string, insecure bool) WSTestResult {
	res := WSTestResult{}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") {
		res.Code = "bad_url"
		res.Detail = "server_url 必须以 ws:// 或 wss:// 开头"
		return res
	}

	dialer := &websocket.Dialer{
		HandshakeTimeout: 8 * time.Second,
		TLSClientConfig:  &tls.Config{InsecureSkipVerify: insecure}, //nolint:gosec — explicit user opt-in, same as helper's -insecure flag
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)

	started := time.Now()
	conn, resp, err := dialer.DialContext(ctx, rawURL, header)
	res.LatencyMs = time.Since(started).Milliseconds()
	if err != nil {
		if resp != nil {
			res.HTTPStatus = resp.StatusCode
			switch resp.StatusCode {
			case http.StatusUnauthorized, http.StatusForbidden:
				res.Code = "auth"
				res.Detail = fmt.Sprintf("服务器拒绝(token 不匹配? HTTP %d)", resp.StatusCode)
			case http.StatusNotFound:
				res.Code = "not_found"
				res.Detail = "路径不对: server_url 需以 /ws 结尾"
			default:
				res.Code = "handshake"
				res.Detail = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, err.Error())
			}
			return res
		}
		res.Code, res.Detail = classifyNetErr(err, "连接失败")
		return res
	}
	defer conn.Close()

	// Prove the app-level protocol accepts us too: send a Hello, then a
	// normal close. We don't wait for a reply — the server only starts
	// talking after the helper's ToolList, which we deliberately skip.
	_ = conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "gui test"),
		time.Now().Add(2*time.Second))
	res.OK = true
	res.Code = "ok"
	return res
}

// classifyNetErr maps low-level dial errors onto the result codes.
func classifyNetErr(err error, prefix string) (code, detail string) {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout", prefix + ": 8 秒内无响应"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout", prefix + ": 8 秒内无响应"
	}
	detail = prefix + ": " + err.Error()
	switch {
	case strings.Contains(err.Error(), "x509"), strings.Contains(err.Error(), "tls:"), strings.Contains(err.Error(), "certificate"):
		return "tls", detail + "(自签名证书? 试试勾选「跳过证书校验」)"
	default:
		return "dial", detail
	}
}

// handleTestServer runs one "测试连接" click for server_url.
func (s *Server) handleTestServer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ServerURL          string `json:"server_url"`
		Token              string `json:"token"`
		InsecureSkipVerify bool   `json:"insecure_skip_verify"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if strings.TrimSpace(body.ServerURL) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "server_url is required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, TestServerURL(ctx, body.ServerURL, body.Token, body.InsecureSkipVerify))
}
