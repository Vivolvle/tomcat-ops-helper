package wsserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"tomcat-ops-helper/internal/protocol"
)

func TestServer_RejectsBadToken(t *testing.T) {
	srv := httptest.NewServer(New(func(string) bool { return false }, nil))
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err == nil {
		c.Close()
		t.Fatal("expected dial to fail with bad token")
	}
}

func TestServer_AcceptsClientAndExposes(t *testing.T) {
	srv := httptest.NewServer(New(func(tok string) bool { return tok == "good" }, nil))
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	conn, _, err := websocket.DefaultDialer.Dial(url, http.Header{"Authorization": []string{"Bearer good"}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Send Hello.
	env := (protocol.Hello{ClientID: "x", Token: "good", Hostname: "h", Version: "v"}).ToEnvelope()
	if err := conn.WriteJSON(env); err != nil {
		t.Fatalf("write hello: %v", err)
	}

	// Wait until the server has read the hello and stored it.
	deadline := time.Now().Add(2 * time.Second)
	var clients []*Client
	for time.Now().Before(deadline) {
		clients = srv.Config.Handler.(*Server).Clients()
		if len(clients) == 1 && clients[0].Hello.ClientID != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(clients) != 1 {
		t.Fatalf("expected 1 client, got %d", len(clients))
	}
	if clients[0].Hello.ClientID != "x" {
		t.Fatalf("hello not stored: %+v", clients[0].Hello)
	}
}

func TestServer_ReAdoptsSameClientIDAfterReconnect(t *testing.T) {
	// Regression: previously, defer s.removeClient(client.ID) captured the
	// temporary c-N id at defer-statement time. After adoptClientID renamed
	// client.ID to the helper-supplied id, the deferred remove ran against
	// the obsolete c-N (no-op) and leaked the real id in s.clients. The
	// next helper that tried to adopt the same id was refused because it
	// looked "taken", and got stuck on c-(N+1) forever.
	srv := httptest.NewServer(New(func(tok string) bool { return tok == "good" }, nil))
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	connect := func(clientID string) {
		conn, _, err := websocket.DefaultDialer.Dial(url, http.Header{"Authorization": []string{"Bearer good"}})
		if err != nil {
			t.Fatalf("dial %s: %v", clientID, err)
		}
		env := (protocol.Hello{ClientID: clientID, Token: "good", Hostname: "h", Version: "v"}).ToEnvelope()
		if err := conn.WriteJSON(env); err != nil {
			t.Fatalf("write hello %s: %v", clientID, err)
		}
		// Read one message to give the server a chance to process the Hello
		// before we close.
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _, _ = conn.ReadMessage()
		conn.Close()
		// Wait for the server's deferred removeClient to run.
		time.Sleep(80 * time.Millisecond)
	}

	connect("benji")
	connect("benji") // second helper, same id — must adopt cleanly

	clients := srv.Config.Handler.(*Server).Clients()
	for _, c := range clients {
		if c.ID == "benji" {
			t.Fatalf("stale benji entry leaked in s.clients: %+v", c)
		}
	}
}

func TestClient_SendAfterClose(t *testing.T) {
	srv := httptest.NewServer(New(func(string) bool { return true }, nil))
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c := &Client{conn: conn}
	_ = c.Close()
	err = c.Send(context.Background(), protocol.Command{ID: "1", Action: "status"})
	if err == nil {
		t.Fatal("expected error sending to closed client")
	}
}

func TestExtractToken(t *testing.T) {
	cases := []struct {
		header string
		query  string
		want   string
	}{
		{"Bearer abc", "", "abc"},
		{"", "token=xyz", "xyz"},
		{"Bearer abc", "token=xyz", "abc"},
		{"", "", ""},
	}
	for _, tc := range cases {
		r, _ := http.NewRequest(http.MethodGet, "/ws", nil)
		if tc.header != "" {
			r.Header.Set("Authorization", tc.header)
		}
		if tc.query != "" {
			r.URL.RawQuery = tc.query
		}
		got := extractToken(r)
		if got != tc.want {
			t.Errorf("extractToken(%q,%q) = %q, want %q", tc.header, tc.query, got, tc.want)
		}
	}
}

func TestServer_ResultEnvelopesMarshal(t *testing.T) {
	res := protocol.Result{ID: "abc", Success: true, Data: json.RawMessage(`{"x":1}`)}
	raw, err := json.Marshal(res.ToEnvelope())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"success":true`) {
		t.Fatalf("success flag missing: %s", raw)
	}
}
