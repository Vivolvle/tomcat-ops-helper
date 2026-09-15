// Package wsserver implements the operator-side WebSocket relay.
package wsserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"tomcat-ops-helper/internal/protocol"
)

// AuthFunc returns the accepted token list. Returning an empty slice denies
// the connection. Returning the supplied token means "ok".
type AuthFunc func(token string) bool

// OnConnect is called once after a client is accepted and has sent its Hello.
// The returned hooks allow the caller to wire command dispatch.
type OnConnect func(client *Client)

// OnHello fires after the helper's Hello envelope has been read and the
// Client.ID has been adopted from Hello.ClientID. Use this for any registry
// that wants to key clients by the helper-supplied id (rather than the
// temporary c-N assigned at upgrade).
type OnHello func(client *Client)

// OnDisconnect is called when a client connection ends (read error or close).
type OnDisconnect func(client *Client)

// ResultFunc is invoked for every Result envelope the helper sends back.
type ResultFunc func(client *Client, result protocol.Result)

// ToolListFunc is invoked when a helper sends its ToolList envelope.
type ToolListFunc func(client *Client, tools protocol.ToolList)

// Server is the wss listener that bridges helper and operator's local agent.
type Server struct {
	upgrader     websocket.Upgrader
	auth         AuthFunc
	onHello      OnHello
	onResult     ResultFunc
	onToolList   ToolListFunc
	onDisconnect OnDisconnect
	mu           sync.Mutex
	clients      map[string]*Client
	connSeq      atomic.Uint64
}

// New builds a Server. auth is required; onConnect and onResult may be nil.
func New(auth AuthFunc, onConnect OnConnect) *Server {
	return &Server{
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			CheckOrigin:     func(r *http.Request) bool { return true },
		},
		auth:    auth,
		clients: map[string]*Client{},
	}
}

// WithHello wires the callback fired after a helper's Hello has been read and
// its supplied client_id has been adopted.
func (s *Server) WithHello(f OnHello) *Server {
	s.onHello = f
	return s
}

// WithResultHandler wires the callback for inbound Result envelopes.
func (s *Server) WithResultHandler(f ResultFunc) *Server {
	s.onResult = f
	return s
}

// WithToolList wires the callback fired when a helper advertises its tools.
func (s *Server) WithToolList(f ToolListFunc) *Server {
	s.onToolList = f
	return s
}

// WithDisconnect wires the callback fired when a client connection ends.
func (s *Server) WithDisconnect(f OnDisconnect) *Server {
	s.onDisconnect = f
	return s
}

// ServeHTTP upgrades the connection, validates the token, and starts the
// client pump.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(strings.ToLower(r.URL.Path), "/ws") {
		http.NotFound(w, r)
		return
	}
	tok := extractToken(r)
	if !s.auth(tok) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	client := s.newClient(conn)
	// NB: capture the *Client, not client.ID — adoptClientID may rename
	// client.ID after the defer is registered. We must remove whatever id the
	// client actually ended up under (the helper-supplied id), otherwise the
	// old c-N entry is a harmless no-op but the real id leaks in s.clients
	// and forever blocks future helpers from adopting the same id.
	defer func() {
		client.mu.Lock()
		id := client.ID
		client.mu.Unlock()
		s.removeClient(id)
	}()

	s.mu.Lock()
	cb := s.onResult
	tcb := s.onToolList
	dcb := s.onDisconnect
	s.mu.Unlock()

	if err := client.pump(cb, tcb, s); err != nil && !errors.Is(err, context.Canceled) {
		// pump already logs internally
	}
	if dcb != nil {
		dcb(client)
	}
}

func extractToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	if t := r.URL.Query().Get("token"); t != "" {
		return t
	}
	return ""
}

// Clients returns a snapshot of connected clients.
func (s *Server) Clients() []*Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Client, 0, len(s.clients))
	for _, c := range s.clients {
		out = append(out, c)
	}
	return out
}

// ClientByToken returns the single client matching token (server tracks only
// one in the deployment scenarios but the API is general).
func (s *Server) ClientByToken(token string) *Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.clients {
		if c.Token == token {
			return c
		}
	}
	return nil
}

// Client represents one connected helper.
type Client struct {
	ID         string
	Token      string
	Hello      protocol.Hello
	HelloAt    time.Time
	conn       *websocket.Conn
	mu         sync.Mutex
	writeWait  time.Duration
	pongWait   time.Duration
	pingPeriod time.Duration
	closed     atomic.Bool
}

func (s *Server) newClient(conn *websocket.Conn) *Client {
	id := fmt.Sprintf("c-%d", s.connSeq.Add(1))
	c := &Client{
		ID:        id,
		conn:      conn,
		writeWait: 10 * time.Second,
		pongWait:  90 * time.Second,
		pingPeriod: 30 * time.Second,
	}
	s.mu.Lock()
	s.clients[id] = c
	s.mu.Unlock()
	return c
}

// adoptClientID replaces the temporary c-N id with the helper-supplied
// client_id from Hello. Called once after the Hello has been read.
func (s *Server) adoptClientID(c *Client, helloID string) {
	if helloID == "" || helloID == c.ID {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, taken := s.clients[helloID]; taken {
		return
	}
	delete(s.clients, c.ID)
	c.ID = helloID
	s.clients[helloID] = c
}

func (s *Server) removeClient(id string) {
	s.mu.Lock()
	delete(s.clients, id)
	s.mu.Unlock()
}

// Send blocks until cmd is written or ctx expires.
func (c *Client) Send(ctx context.Context, cmd protocol.Command) error {
	if c.closed.Load() {
		return errors.New("wsserver: client closed")
	}
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return errors.New("wsserver: no live conn")
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(c.writeWait)
	}
	_ = conn.SetWriteDeadline(deadline)
	if err := conn.WriteJSON(cmd.ToEnvelope()); err != nil {
		c.closed.Store(true)
		return err
	}
	return nil
}

// Close terminates the client.
func (c *Client) Close() error {
	c.closed.Store(true)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

// pump reads from the helper, keeping connection state fresh. Hello must be
// the first message; subsequent messages are dispatched via the configured
// handler.
func (c *Client) pump(serverResult ResultFunc, serverToolList ToolListFunc, server *Server) error {
	c.conn.SetReadLimit(1 << 24) // 16 MiB
	_ = c.conn.SetReadDeadline(time.Now().Add(c.pongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(c.pongWait))
	})

	pingTicker := time.NewTicker(c.pingPeriod)
	defer pingTicker.Stop()

	helloCh := make(chan error, 1)
	go func() {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			helloCh <- err
			return
		}
		env, err := protocol.DecodeEnvelope(raw)
		if err != nil || env.Type != protocol.TypeHello {
			helloCh <- fmt.Errorf("wsserver: expected hello, got %v (%w)", env.Type, err)
			return
		}
		h, err := protocol.HelloFromEnvelope(env)
		if err != nil {
			helloCh <- err
			return
		}
		c.mu.Lock()
		c.Hello = h
		c.Token = h.Token
		c.HelloAt = time.Now().UTC()
		c.mu.Unlock()
		if server != nil {
			server.adoptClientID(c, h.ClientID)
		}
		helloCh <- nil
	}()

	// Send initial ping sequence; pong will keep things alive.
	go func() {
		for range pingTicker.C {
			c.mu.Lock()
			conn := c.conn
			c.mu.Unlock()
			if conn == nil {
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(c.writeWait))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}()

	if err := <-helloCh; err != nil {
		return err
	}
	if server != nil && server.onHello != nil {
		server.onHello(c)
	}
	return c.drain(serverResult, serverToolList)
}

// drain consumes results and tool lists until the connection drops.
func (c *Client) drain(serverResult ResultFunc, serverToolList ToolListFunc) error {
	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			return err
		}
		env, err := protocol.DecodeEnvelope(raw)
		if err != nil {
			continue
		}
		switch env.Type {
		case protocol.TypeResult:
			res, err := protocol.ResultFromEnvelope(env)
			if err != nil {
				continue
			}
			if serverResult != nil {
				serverResult(c, res)
			}
		case protocol.TypeToolList:
			tl, err := protocol.ToolListFromEnvelope(env)
			if err != nil {
				continue
			}
			if serverToolList != nil {
				serverToolList(c, tl)
			}
		}
	}
}
