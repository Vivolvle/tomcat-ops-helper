package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"tomcat-ops-helper/internal/protocol"
	"tomcat-ops-helper/internal/wsserver"
)

// TestMcpEndToEnd wires the real wsserver.ServeHTTP behind httptest, drives a
// helper through wsclient, and exercises the /mcp/tools + /mcp/call flow.
func TestMcpEndToEnd(t *testing.T) {
	var (
		mu        sync.Mutex
		seenTools []protocol.ToolMeta
		gotCmd    protocol.Command
	)

	relay := newTestRelay(t)
	wsHandler := wsserver.New(relay.auth, func(c *wsserver.Client) {}).
		WithHello(func(c *wsserver.Client) {
			relay.register(c)
		}).
		WithResultHandler(func(c *wsserver.Client, r protocol.Result) {
			relay.deliver(c, r)
		}).
		WithToolList(func(c *wsserver.Client, tl protocol.ToolList) {
			relay.storeTools(c, tl)
			mu.Lock()
			seenTools = tl.Tools
			mu.Unlock()
		}).
		WithDisconnect(func(c *wsserver.Client) { relay.unregister(c) })

	mux := http.NewServeMux()
	mux.Handle("/ws", wsHandler)
	mux.HandleFunc("/mcp/tools", relay.handleMcpTools)
	mux.HandleFunc("/mcp/call", relay.handleMcpCall)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	u.Scheme = "ws"
	u.Path = "/ws"
	wsURL := u.String()

	d := websocket.Dialer{}
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer tok-1234567890")
	conn, _, err := d.Dial(wsURL, hdr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// Token must satisfy relay.auth ("tok-1234567890" is accepted).
	_ = conn.WriteJSON((protocol.Hello{ClientID: "test", Token: "tok-1234567890"}).ToEnvelope())
	_ = conn.WriteJSON((protocol.ToolList{Tools: []protocol.ToolMeta{
		{Name: "status", Description: "x", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "echo", Description: "y", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}}).ToEnvelope())

	// Wait for tool list to register.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := len(seenTools) > 0
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	if len(seenTools) != 2 {
		mu.Unlock()
		t.Fatalf("tool list not registered; got %d", len(seenTools))
	}
	mu.Unlock()

	// Pump commands from the server side and reply.
	gotCmdCh := make(chan struct{}, 1)
	go func() {
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			env, _ := protocol.DecodeEnvelope(raw)
			if env.Type != protocol.TypeCommand {
				continue
			}
			cmd, _ := protocol.CommandFromEnvelope(env)
			mu.Lock()
			gotCmd = cmd
			mu.Unlock()
			res := protocol.Result{ID: cmd.ID, Success: true, Data: json.RawMessage(`{"hostname":"testbox"}`)}
			_ = conn.WriteJSON(res.ToEnvelope())
			select {
			case gotCmdCh <- struct{}{}:
			default:
			}
		}
	}()

	// Call a tool via JSON-RPC.
	body := `{"jsonrpc":"2.0","id":"x1","method":"tools/call","params":{"name":"status","client_id":"test"}}`
	resp, err := http.Post(srv.URL+"/mcp/call", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	rawBody, _ := io.ReadAll(resp.Body)
	t.Logf("response: %s", rawBody)

	var rpc struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  struct {
			Content []map[string]any `json:"content"`
			Data    json.RawMessage  `json:"data"`
		} `json:"result"`
		Error *jsonrpcError `json:"error"`
	}
	if err := json.Unmarshal(rawBody, &rpc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rpc.Error != nil {
		t.Fatalf("unexpected error: %+v", rpc.Error)
	}
	if rpc.ID.String() != `"x1"` {
		t.Fatalf("id echo = %s", rpc.ID.String())
	}
	if !strings.Contains(string(rpc.Result.Data), `"hostname":"testbox"`) {
		t.Fatalf("data mismatch: %s", rpc.Result.Data)
	}
	select {
	case <-gotCmdCh:
	case <-time.After(2 * time.Second):
		t.Fatal("server never sent the command to the helper")
	}
	mu.Lock()
	if gotCmd.Action != "status" {
		mu.Unlock()
		t.Fatalf("helper got action = %s", gotCmd.Action)
	}
	mu.Unlock()

	// And unknown tool should be rejected by /mcp/call.
	bad := `{"jsonrpc":"2.0","id":"x2","method":"tools/call","params":{"name":"missing","client_id":"test"}}`
	resp2, err := http.Post(srv.URL+"/mcp/call", "application/json", strings.NewReader(bad))
	if err != nil {
		t.Fatalf("post2: %v", err)
	}
	defer resp2BodyClose(resp2)
	var rpcErr jsonrpcResponse
	_ = json.NewDecoder(resp2.Body).Decode(&rpcErr)
	if rpcErr.Error == nil {
		t.Fatalf("expected error for unknown tool")
	}

	_ = conn.Close()
}

func resp2BodyClose(r *http.Response) { _ = r.Body.Close() }