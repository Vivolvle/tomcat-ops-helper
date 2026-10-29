package wsclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"tomcat-ops-helper/internal/protocol"
)

func newTestServer(t *testing.T, handler http.Handler) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewServer(handler)
	u, _ := url.Parse(srv.URL)
	u.Scheme = "ws"
	u.Path = "/ws"
	return srv, u.String()
}

func TestClient_ConnectsAndDispatches(t *testing.T) {
	var sawHello atomic.Pointer[protocol.Hello]
	resultsCh := make(chan protocol.Result, 4)
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-12345" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		env, _ := protocol.DecodeEnvelope(raw)
		h, _ := protocol.HelloFromEnvelope(env)
		sawHello.Store(&h)
		// Send a command.
		cmd := protocol.Command{ID: "rid-1", Action: "status"}
		_ = conn.WriteJSON(cmd.ToEnvelope())
		// Read result.
		_, raw, err = conn.ReadMessage()
		if err != nil {
			return
		}
		env, _ = protocol.DecodeEnvelope(raw)
		res, _ := protocol.ResultFromEnvelope(env)
		resultsCh <- res
	})
	srv, wsURL := newTestServer(t, mux)
	defer srv.Close()

	c, err := New(Options{
		URL:                wsURL,
		Token:              "secret-12345",
		InsecureSkipVerify: true,
		Hello: func() protocol.Hello {
			return protocol.Hello{ClientID: "c1", Token: "secret-12345", Hostname: "h", Version: "t"}
		},
		Handler: func(ctx context.Context, cmd protocol.Command) protocol.Result {
			return protocol.Result{ID: cmd.ID, Success: true, Data: json.RawMessage(`{"ok":true}`)}
		},
		ReconnectInterval: 50 * time.Millisecond,
		HeartbeatInterval: 50 * time.Millisecond,
		HeartbeatTimeout:  500 * time.Millisecond,
		DialTimeout:       2 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go c.Run(ctx)

	select {
	case res := <-resultsCh:
		if res.ID != "rid-1" || !res.Success {
			t.Fatalf("unexpected result: %+v", res)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for result")
	}

	h := sawHello.Load()
	if h == nil || h.ClientID != "c1" {
		t.Fatalf("hello not received correctly: %v", h)
	}
	_ = c.Close()
}

func TestClient_RejectsBadToken(t *testing.T) {
	gotToken := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		// Non-blocking send so a flood of reconnects does not stall the
		// handler goroutine and keep the HTTP connection open.
		select {
		case gotToken <- r.Header.Get("Authorization"):
		default:
		}
		http.Error(w, "forbidden", http.StatusForbidden)
	})
	srv, wsURL := newTestServer(t, mux)
	defer srv.Close()

	c, _ := New(Options{
		URL:   wsURL,
		Token: "wrong-token",
		Hello: func() protocol.Hello { return protocol.Hello{ClientID: "x", Token: "wrong-token"} },
		Handler: func(ctx context.Context, cmd protocol.Command) protocol.Result {
			return protocol.Result{}
		},
		ReconnectInterval: 50 * time.Millisecond,
		DialTimeout:       500 * time.Millisecond,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := c.Run(ctx)
	if err == nil {
		t.Fatal("expected error from Run on cancellation")
	}
	select {
	case h := <-gotToken:
		if !strings.Contains(h, "wrong-token") {
			t.Fatalf("token header wrong: %s", h)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("server never got request")
	}
}

func TestClient_ReconnectsAfterServerDrop(t *testing.T) {
	var drops atomic.Int32
	mux := http.NewServeMux()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		drops.Add(1)
		_, _, _ = conn.ReadMessage()
		// Drop the connection mid-life.
		_ = conn.Close()
	})
	srv, wsURL := newTestServer(t, mux)
	defer srv.Close()

	c, _ := New(Options{
		URL:                wsURL,
		Token:              "ok",
		InsecureSkipVerify: true,
		Hello:              func() protocol.Hello { return protocol.Hello{ClientID: "x", Token: "ok"} },
		Handler: func(ctx context.Context, cmd protocol.Command) protocol.Result {
			return protocol.Result{}
		},
		ReconnectInterval: 50 * time.Millisecond,
		HeartbeatInterval: 30 * time.Millisecond,
		HeartbeatTimeout:  200 * time.Millisecond,
		DialTimeout:       1 * time.Second,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = c.Run(ctx) }()
	// Wait for at least 2 connection attempts.
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if drops.Load() >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if drops.Load() < 2 {
		t.Fatalf("expected >=2 reconnects, got %d", drops.Load())
	}
	cancel()
	wg.Wait()
}

func TestNew_RequiresFields(t *testing.T) {
	cases := []Options{
		{},
		{URL: "ws://x", Handler: func(context.Context, protocol.Command) protocol.Result { return protocol.Result{} }},
		{URL: "ws://x", Hello: func() protocol.Hello { return protocol.Hello{} }},
	}
	for i, o := range cases {
		if _, err := New(o); err == nil {
			t.Fatalf("case %d: expected error", i)
		}
	}
}

// Concurrent replies, tool-list pushes and heartbeats share one writer.
func TestClient_ConcurrentWrites(t *testing.T) {
	const count = 200
	received := make(chan int, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for i := 0; i < count+1; i++ {
			if _, _, err := conn.ReadMessage(); err != nil {
				received <- i
				return
			}
		}
		received <- count + 1
	})
	srv, wsURL := newTestServer(t, mux)
	defer srv.Close()
	c, err := New(Options{URL: wsURL, Hello: func() protocol.Hello { return protocol.Hello{} }, Handler: func(context.Context, protocol.Command) protocol.Result { return protocol.Result{} }, HeartbeatInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = c.Run(ctx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		c.mu.Lock()
		connected := c.conn != nil
		c.mu.Unlock()
		if connected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("connection timeout")
		}
		time.Sleep(time.Millisecond)
	}
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.PushToolList(nil); err != nil {
				t.Errorf("push: %v", err)
			}
		}()
	}
	wg.Wait()
	select {
	case n := <-received:
		if n != count+1 {
			t.Fatalf("received %d frames", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out receiving frames")
	}
}

// TestRun_CtxCancel_UnblocksReadLoop reproduces the "Ctrl+C hangs for
// HeartbeatTimeout" regression. The server hammers the conn with WebSocket
// pings fast enough that the client's pong handler would otherwise keep
// resetting the read deadline forever. After cancelling the ctx, Run must
// return promptly — not after the full HeartbeatTimeout window.
func TestRun_CtxCancel_UnblocksReadLoop(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		// Keep sending WS-level pings faster than HeartbeatTimeout so the
		// client's read deadline gets reset continuously.
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		go func() {
			for range ticker.C {
				_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
				if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					return
				}
			}
		}()
		// Read forever (until client closes).
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	srv, wsURL := newTestServer(t, mux)
	defer srv.Close()

	c, err := New(Options{
		URL:                wsURL,
		Token:              "ok",
		InsecureSkipVerify: true,
		Hello:              func() protocol.Hello { return protocol.Hello{ClientID: "x", Token: "ok"} },
		Handler:            func(context.Context, protocol.Command) protocol.Result { return protocol.Result{} },
		HeartbeatInterval:  50 * time.Millisecond,
		HeartbeatTimeout:   2 * time.Second, // would be the slow path
		ReconnectInterval:  50 * time.Millisecond,
		DialTimeout:        time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()

	// Give the client time to connect and start receiving pings.
	time.Sleep(300 * time.Millisecond)

	t0 := time.Now()
	cancel()

	select {
	case err := <-runDone:
		elapsed := time.Since(t0)
		if elapsed > 500*time.Millisecond {
			t.Fatalf("Run took %s to return after cancel — ctx-aware read hook failed (got err=%v)", elapsed, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return within 3s after ctx cancel — deadlock")
	}
}
