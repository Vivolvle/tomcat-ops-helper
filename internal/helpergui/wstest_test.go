package helpergui

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func wsUpgrader() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		// Real server just waits for Hello; tolerate the early close.
		_, _, _ = c.ReadMessage()
	})
}

func TestWSTestSuccess(t *testing.T) {
	ts := httptest.NewServer(wsUpgrader())
	defer ts.Close()
	r := TestServerURL(context.Background(), "ws"+ts.URL[4:], "token123", false)
	if !r.OK || r.Code != "ok" {
		t.Fatalf("want ok, got %+v", r)
	}
}

func TestWSTestAuthFail(t *testing.T) {
	// The project's own server rejects a bad token with 403.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer ts.Close()
	r := TestServerURL(context.Background(), "ws"+ts.URL[4:], "bad", false)
	if r.Code != "auth" || r.HTTPStatus != 403 {
		t.Fatalf("want auth/403, got %+v", r)
	}
}

func TestWSTestNotFound(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	defer ts.Close()
	r := TestServerURL(context.Background(), "ws"+ts.URL[4:], "token123", false)
	if r.Code != "not_found" {
		t.Fatalf("want not_found, got %+v", r)
	}
}

func TestWSTestNoUpgrade(t *testing.T) {
	// Plain 200 without an Upgrade header.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hi"))
	}))
	defer ts.Close()
	r := TestServerURL(context.Background(), "ws"+ts.URL[4:], "token123", false)
	if r.Code != "handshake" {
		t.Fatalf("want handshake, got %+v", r)
	}
}

func TestWSTestDialFail(t *testing.T) {
	// Bind then close: a port that is (almost certainly) refused.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	port := l.Addr().String()
	l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := TestServerURL(ctx, "ws://"+port, "token123", false)
	if r.OK || r.Code == "" {
		t.Fatalf("want a failure code, got %+v", r)
	}
	if r.Code != "dial" && r.Code != "timeout" {
		t.Fatalf("want dial or timeout, got %+v", r)
	}
}

func TestWSTestBadURL(t *testing.T) {
	r := TestServerURL(context.Background(), "https://not-websocket", "", false)
	if r.Code != "bad_url" {
		t.Fatalf("want bad_url, got %+v", r)
	}
}
