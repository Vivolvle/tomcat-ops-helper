// Package wsclient implements the reverse-WS helper side.
package wsclient

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"tomcat-ops-helper/internal/protocol"
)

// HelloSender produces the initial Hello payload sent right after upgrade.
type HelloSender func() protocol.Hello

// PostConnect runs once after the Hello has been accepted by the server. The
// client is given the live connection so it can send additional envelopes
// (e.g. a tool list). Returning an error aborts the session.
type PostConnect func(conn interface {
	WriteJSON(v any) error
}) error

// CommandHandler processes a single command and returns a Result. Errors
// are converted to success=false and surfaced in the Result envelope.
type CommandHandler func(ctx context.Context, cmd protocol.Command) protocol.Result

// Options tunes the client. Zero values are filled with sensible defaults.
type Options struct {
	URL                string
	Token              string
	InsecureSkipVerify bool
	Hello              HelloSender
	PostConnect        PostConnect
	Handler            CommandHandler
	Logger             *log.Logger
	HeartbeatInterval  time.Duration
	HeartbeatTimeout   time.Duration
	ReconnectInterval  time.Duration
	DialTimeout        time.Duration
	WriteTimeout       time.Duration
	ReadTimeout        time.Duration
}

// Client maintains one logical connection, reconnecting on failure.
type Client struct {
	opts    Options
	mu      sync.Mutex
	conn    *websocket.Conn
	closed  atomic.Bool
	current atomic.Pointer[protocol.Hello]
}

// New creates a Client. Handler and HelloSender are required.
func New(opts Options) (*Client, error) {
	if opts.Handler == nil {
		return nil, errors.New("wsclient: Handler is required")
	}
	if opts.Hello == nil {
		return nil, errors.New("wsclient: HelloSender is required")
	}
	if opts.URL == "" {
		return nil, errors.New("wsclient: URL is required")
	}
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}
	if opts.HeartbeatInterval <= 0 {
		opts.HeartbeatInterval = 30 * time.Second
	}
	if opts.HeartbeatTimeout <= 0 {
		opts.HeartbeatTimeout = 90 * time.Second
	}
	if opts.ReconnectInterval <= 0 {
		opts.ReconnectInterval = 5 * time.Second
	}
	if opts.DialTimeout <= 0 {
		opts.DialTimeout = 15 * time.Second
	}
	if opts.WriteTimeout <= 0 {
		opts.WriteTimeout = 10 * time.Second
	}
	return &Client{opts: opts}, nil
}

// Run blocks until ctx is cancelled. It loops: connect, dispatch, reconnect.
func (c *Client) Run(ctx context.Context) error {
	for {
		if err := c.connectAndServe(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			c.opts.Logger.Printf("wsclient: connection ended: %v; reconnecting in %s", err, c.opts.ReconnectInterval)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(c.opts.ReconnectInterval):
				continue
			}
		}
	}
}

// Close shuts down the active connection, if any.
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

func (c *Client) connectAndServe(ctx context.Context) error {
	dialer := &websocket.Dialer{
		HandshakeTimeout: c.opts.DialTimeout,
		TLSClientConfig:  &tls.Config{InsecureSkipVerify: c.opts.InsecureSkipVerify},
	}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+c.opts.Token)

	conn, _, err := dialer.DialContext(ctx, c.opts.URL, headers)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		if c.conn == conn {
			c.conn = nil
		}
		c.mu.Unlock()
		_ = conn.Close()
	}()

	hello := c.opts.Hello()
	c.current.Store(&hello)
	if err := c.writeJSON(conn, hello.ToEnvelope()); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}
	if c.opts.PostConnect != nil {
		if err := c.opts.PostConnect(conn); err != nil {
			return fmt.Errorf("post-connect: %w", err)
		}
	}

	// Read loop. Also runs the heartbeat side.
	conn.SetReadDeadline(time.Now().Add(c.opts.HeartbeatTimeout))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(c.opts.HeartbeatTimeout))
		return nil
	})

	// Force ReadMessage to return immediately when ctx is cancelled. Without
	// this, server-side WebSocket pings reset the read deadline on every
	// pong, so Ctrl+C would have to wait the full HeartbeatTimeout (90s)
	// before the read loop noticed. Setting the deadline to now causes
	// ReadMessage to return "i/o timeout" within microseconds.
	go func() {
		<-ctx.Done()
		_ = conn.SetReadDeadline(time.Now())
	}()

	pingStop := make(chan struct{})
	pingDone := make(chan struct{})
	go c.heartbeat(conn, pingStop, pingDone)

	defer func() {
		close(pingStop)
		<-pingDone
	}()

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		conn.SetReadDeadline(time.Now().Add(c.opts.HeartbeatTimeout))
		env, err := protocol.DecodeEnvelope(raw)
		if err != nil {
			c.opts.Logger.Printf("wsclient: bad envelope: %v", err)
			continue
		}
		switch env.Type {
		case protocol.TypePing:
			if err := c.writeJSON(conn, &protocol.Envelope{Type: protocol.TypePong}); err != nil {
				return fmt.Errorf("write pong: %w", err)
			}
		case protocol.TypeCommand:
			cmd, err := protocol.CommandFromEnvelope(env)
			if err != nil {
				c.opts.Logger.Printf("wsclient: bad command: %v", err)
				continue
			}
			go c.dispatchAndReply(ctx, conn, cmd)
		default:
			c.opts.Logger.Printf("wsclient: ignoring message type %q", env.Type)
		}
	}
}

func (c *Client) dispatchAndReply(ctx context.Context, conn *websocket.Conn, cmd protocol.Command) {
	res := c.opts.Handler(ctx, cmd)
	if err := c.writeJSON(conn, res.ToEnvelope()); err != nil {
		c.opts.Logger.Printf("wsclient: write result %s: %v", cmd.ID, err)
	}
}

func (c *Client) heartbeat(conn *websocket.Conn, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	t := time.NewTicker(c.opts.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			c.mu.Lock()
			cur := c.conn
			c.mu.Unlock()
			if cur == nil || cur != conn {
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(c.opts.WriteTimeout))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (c *Client) writeJSON(conn *websocket.Conn, v any) error {
	if c.opts.WriteTimeout > 0 {
		_ = conn.SetWriteDeadline(time.Now().Add(c.opts.WriteTimeout))
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, b)
}

// Current returns the latest Hello sent to the server, if any.
func (c *Client) Current() (protocol.Hello, bool) {
	p := c.current.Load()
	if p == nil {
		return protocol.Hello{}, false
	}
	return *p, true
}
