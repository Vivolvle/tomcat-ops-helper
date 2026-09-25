// server is the operator-side WebSocket relay. It accepts reverse wss
// connections from helpers, validates tokens, and exposes a small HTTP bridge
// for the local agent to dispatch commands and read results.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"tomcat-ops-helper/internal/config"
	"tomcat-ops-helper/internal/protocol"
	"tomcat-ops-helper/internal/wsserver"
)

const serverVersion = "0.1.0"

func main() {
	var (
		configPath = flag.String("config", "server.json", "path to server.json")
	)
	flag.Parse()

	cfg, err := config.LoadServer(*configPath)
	if err != nil {
		fatal(fmt.Errorf("load server config: %w", err))
	}

	logger := log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds)
	logger.Printf("server %s starting; listen=%s ws_listen=%q agent_listen=%s problem_log=%s",
		serverVersion, cfg.Listen, cfg.WSListen, cfg.AgentListen, cfg.ProblemLog)

	auditFile, err := os.OpenFile(cfg.AuditLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fatal(fmt.Errorf("open audit log: %w", err))
	}
	defer auditFile.Close()
	audit := func(entry map[string]any) {
		b, _ := json.Marshal(entry)
		b = append(b, '\n')
		_, _ = auditFile.Write(b)
	}

	relay := newRelay(cfg, logger, audit)

	wsHandler := wsserver.New(relay.auth, func(client *wsserver.Client) {
		// reserved for future audit hooks; registration happens after Hello so
		// the helper-supplied client_id is the key.
	}).
		WithHello(func(client *wsserver.Client) {
			relay.register(client)
		}).
		WithResultHandler(func(client *wsserver.Client, res protocol.Result) {
			relay.deliver(client, res)
		}).
		WithToolList(func(client *wsserver.Client, tl protocol.ToolList) {
			relay.storeTools(client, tl)
		}).
		WithDisconnect(func(client *wsserver.Client) {
			relay.unregister(client)
		})
	tlsServer := &http.Server{
		Addr:      cfg.Listen,
		Handler:   wsHandler,
		TLSConfig: nil,
	}

	var wsServer *http.Server
	if cfg.WSListen != "" {
		wsServer = &http.Server{
			Addr:              cfg.WSListen,
			Handler:           wsHandler,
			ReadHeaderTimeout: 5 * time.Second,
		}
	}

	agentMux := http.NewServeMux()
	agentMux.HandleFunc("/clients", relay.handleListClients)
	agentMux.HandleFunc("/command", relay.handleCommand)
	agentMux.HandleFunc("/health", relay.handleHealth)
	agentMux.HandleFunc("/mcp/tools", relay.handleMcpTools)
	agentMux.HandleFunc("/mcp/call", relay.handleMcpCall)
	agentMux.HandleFunc("/upload", relay.handleUpload)
	agentMux.HandleFunc("/upload/", relay.handleUploadProgress)
	agentServer := &http.Server{
		Addr:              cfg.AgentListen,
		Handler:           agentMux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errs := make(chan error, 3)
	go func() {
		logger.Printf("server: wss listening on %s", cfg.Listen)
		errs <- tlsServer.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
	}()
	go func() {
		logger.Printf("server: agent http listening on %s", cfg.AgentListen)
		errs <- agentServer.ListenAndServe()
	}()
	if wsServer != nil {
		go func() {
			logger.Printf("server: plain ws listening on %s (for cloudflared/quick tunnels)", cfg.WSListen)
			errs <- wsServer.ListenAndServe()
		}()
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	select {
	case s := <-sigs:
		logger.Printf("server: signal %s received, shutting down", s)
	case err := <-errs:
		if err != nil && err != http.ErrServerClosed {
			logger.Printf("server: listener exited: %v", err)
		}
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = tlsServer.Shutdown(shutdownCtx)
	_ = agentServer.Shutdown(shutdownCtx)
	if wsServer != nil {
		_ = wsServer.Shutdown(shutdownCtx)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "server fatal:", err)
	os.Exit(1)
}

// uploadProgressInfo tracks the progress of an in-flight upload.
type uploadProgressInfo struct {
	TotalBytes   int64
	WrittenBytes int64
	Path         string
	ClientID     string
	StartedAt    time.Time
	LastUpdate   time.Time
}

// pendingCall is one in-flight Command awaiting its Result.
type pendingCall struct {
	ch       chan protocol.Result
	clientID string
}

// defaultCallTimeout is how long /command and /mcp/call wait for a helper
// Result when the request doesn't carry an explicit timeout. It is
// deliberately generous (300s, up from 30s as of 2026-09-25): slow actions
// such as deploy bounded by the helper still return a structured result
// ("exec timeout after 30s", per-step DeployData, …) that the caller can
// act on, while a short server-side cutoff replaced all of that with a
// bare "timeout" and no visibility. Callers that want less can pass
// timeout explicitly.
const defaultCallTimeout = 300 * time.Second

// relay wires client registration, command dispatch, and result collection.
type relay struct {
	cfg    *config.ServerConfig
	logger *log.Logger
	audit  func(map[string]any)

	mu      sync.Mutex
	clients map[string]*wsserver.Client
	pending map[string]pendingCall
	tools   map[string][]protocol.ToolMeta // keyed by client_id

	// uploadProgress tracks in-flight uploads for progress queries.
	uploadProgress   map[string]*uploadProgressInfo
	uploadProgressMu sync.Mutex

	// problemMu serializes appends to the server-local problem log.
	problemMu sync.Mutex
}

func newRelay(cfg *config.ServerConfig, logger *log.Logger, audit func(map[string]any)) *relay {
	return &relay{
		cfg:            cfg,
		logger:         logger,
		audit:          audit,
		clients:        map[string]*wsserver.Client{},
		pending:        map[string]pendingCall{},
		tools:          map[string][]protocol.ToolMeta{},
		uploadProgress: map[string]*uploadProgressInfo{},
	}
}

func (r *relay) auth(token string) bool {
	return r.cfg.HasToken(token)
}

func (r *relay) register(c *wsserver.Client) {
	r.mu.Lock()
	r.clients[c.ID] = c
	r.mu.Unlock()
	r.audit(map[string]any{
		"event":     "connect",
		"client_id": c.ID,
		"hostname":  c.Hello.Hostname,
		"version":   c.Hello.Version,
		"ts":        time.Now().UTC().Format(time.RFC3339Nano),
	})
}

func (r *relay) unregister(c *wsserver.Client) {
	r.mu.Lock()
	delete(r.clients, c.ID)
	delete(r.tools, c.ID)
	// With the long default call timeout, a helper dropping mid-command
	// would otherwise park every in-flight HTTP caller for the full 300s.
	// Resolve them right here with a synthetic error Result. Non-blocking
	// send: the waiting side may have already timed out and drained.
	for id, pc := range r.pending {
		if pc.clientID == c.ID {
			delete(r.pending, id)
			pc.ch <- protocol.Result{ID: id, Error: "helper disconnected before result"}
		}
	}
	r.mu.Unlock()
	r.audit(map[string]any{
		"event":     "disconnect",
		"client_id": c.ID,
		"ts":        time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// deliver routes an inbound Result to the channel waiting for it. Pending
// requests that have already been abandoned (timed out, client gone) are
// silently dropped.
func (r *relay) deliver(c *wsserver.Client, res protocol.Result) {
	r.mu.Lock()
	pc, ok := r.pending[res.ID]
	if ok {
		delete(r.pending, res.ID)
	}
	r.mu.Unlock()
	if ok {
		pc.ch <- res
	}
}

// storeTools records the tool list advertised by a connected helper. The
// server uses this to serve /mcp/tools without forwarding a discovery call.
func (r *relay) storeTools(c *wsserver.Client, tl protocol.ToolList) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[c.ID] = tl.Tools
	r.audit(map[string]any{
		"event":     "toollist",
		"client_id": c.ID,
		"count":     len(tl.Tools),
		"ts":        time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// handleMcpTools implements GET /mcp/tools. If client_id is supplied it
// returns the tool list for that helper; otherwise it returns an aggregated
// view across all connected helpers (each tagged by client_id).
func (r *relay) handleMcpTools(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cid := req.URL.Query().Get("client_id")
	if cid != "" {
		tools, ok := r.tools[cid]
		if !ok {
			http.Error(w, "unknown client_id or no tool list yet", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"client_id":    cid,
			"server_tools": serverToolDefs(),
			"tools":        tools,
		})
		return
	}
	aggregated := make([]map[string]any, 0, len(r.tools))
	for id, tools := range r.tools {
		aggregated = append(aggregated, map[string]any{
			"client_id": id,
			"tools":     tools,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"server_tools": serverToolDefs(),
		"clients":      aggregated,
	})
}

// handleMcpCall implements POST /mcp/call. Body is JSON-RPC 2.0:
//
//	{
//	  "jsonrpc":"2.0",
//	  "id":"req-1",
//	  "method":"tools/call",
//	  "params":{"name":"status","arguments":{},"client_id":"c-1","timeout":30}
//	}
//
// The server wraps the call as a Command, forwards it to the chosen helper
// (or the only connected one if client_id is omitted), waits for the matching
// Result, and returns a JSON-RPC response.
func (r *relay) handleMcpCall(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var env jsonrpcRequest
	if err := json.NewDecoder(req.Body).Decode(&env); err != nil {
		writeJSONRPCError(w, nil, -32700, "parse error", err.Error())
		return
	}
	if env.JSONRPC != "2.0" {
		writeJSONRPCError(w, env.ID, -32600, "invalid jsonrpc version", "must be 2.0")
		return
	}
	if env.Method != "tools/call" {
		writeJSONRPCError(w, env.ID, -32601, "method not found", env.Method)
		return
	}
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		ClientID  string          `json:"client_id"`
		Timeout   float64         `json:"timeout"`
	}
	if err := json.Unmarshal(env.Params, &p); err != nil {
		writeJSONRPCError(w, env.ID, -32602, "invalid params", err.Error())
		return
	}
	if p.Name == "" {
		writeJSONRPCError(w, env.ID, -32602, "missing tool name", "")
		return
	}

	// Server-local tools run here, with no helper round-trip and no
	// client_id requirement (they must work with zero helpers connected).
	if isServerTool(p.Name) {
		data, text, err := r.executeServerTool(p.Name, p.Arguments)
		if err != nil {
			code := -32000
			if errors.Is(err, errInvalidArguments) {
				code = -32602
			}
			writeJSONRPCError(w, env.ID, code, "server tool failed", err.Error())
			return
		}
		r.audit(map[string]any{
			"event": "server_tool",
			"tool":  p.Name,
			"ts":    time.Now().UTC().Format(time.RFC3339Nano),
		})
		writeJSONRPCResult(w, env.ID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
			"data":    json.RawMessage(data),
		})
		return
	}

	r.mu.Lock()
	if p.ClientID == "" {
		switch len(r.clients) {
		case 0:
			r.mu.Unlock()
			writeJSONRPCError(w, env.ID, -32004, "no helper connected", "")
			return
		case 1:
			for id := range r.clients {
				p.ClientID = id
			}
		default:
			r.mu.Unlock()
			writeJSONRPCError(w, env.ID, -32004, "client_id required",
				fmt.Sprintf("%d helpers connected", len(r.clients)))
			return
		}
	}
	client, ok := r.clients[p.ClientID]
	if !ok {
		r.mu.Unlock()
		writeJSONRPCError(w, env.ID, -32004, "unknown client_id", p.ClientID)
		return
	}
	tools, hasTools := r.tools[p.ClientID]
	if hasTools && !toolListContains(tools, p.Name) {
		r.mu.Unlock()
		writeJSONRPCError(w, env.ID, -32602, "tool not advertised", p.Name)
		return
	}
	r.mu.Unlock()

	timeout := time.Duration(p.Timeout) * time.Second
	if timeout <= 0 {
		timeout = defaultCallTimeout
	}
	id := protocol.NewID()
	cmd := protocol.Command{ID: id, Action: p.Name, Params: p.Arguments}

	resCh := make(chan protocol.Result, 1)
	r.mu.Lock()
	r.pending[id] = pendingCall{ch: resCh, clientID: client.ID}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.pending, id)
		r.mu.Unlock()
	}()

	if err := client.Send(req.Context(), cmd); err != nil {
		r.unregister(client)
		writeJSONRPCError(w, env.ID, -32003, "send failed", err.Error())
		return
	}
	r.audit(map[string]any{
		"event":     "mcp_call",
		"client_id": client.ID,
		"tool":      p.Name,
		"id":        id,
		"ts":        time.Now().UTC().Format(time.RFC3339Nano),
	})

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res := <-resCh:
		if res.Success {
			writeJSONRPCResult(w, env.ID, map[string]any{
				"content": []map[string]any{{"type": "text", "text": string(res.Data)}},
				"data":    json.RawMessage(res.Data),
			})
		} else {
			// Put the helper's reason in the message itself, not just in
			// `data`: MCP clients surface `message` to the model and the
			// 2026-09-15 report showed the real cause ("work dir not
			// allowed: …") was invisible for six straight calls.
			msg := "tool execution failed"
			if s := strings.TrimSpace(res.Error); s != "" {
				msg += ": " + s
			}
			writeJSONRPCError(w, env.ID, -32000, msg, res.Error)
		}
	case <-timer.C:
		writeJSONRPCError(w, env.ID, -32001, "timeout", "")
	}
}

// toolListContains reports whether name appears in the slice.
func toolListContains(tools []protocol.ToolMeta, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

// jsonrpcRequest is the minimal JSON-RPC 2.0 envelope we accept.
type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data,omitempty"`
}

func writeJSONRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(jsonrpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func writeJSONRPCError(w http.ResponseWriter, id json.RawMessage, code int, msg, data string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(jsonrpcResponse{JSONRPC: "2.0", ID: id, Error: &jsonrpcError{Code: code, Message: msg, Data: data}})
}

// handleListClients returns the registered clients as JSON.
func (r *relay) handleListClients(w http.ResponseWriter, _ *http.Request) {
	r.mu.Lock()
	out := make([]map[string]any, 0, len(r.clients))
	for _, c := range r.clients {
		out = append(out, map[string]any{
			"id":           c.ID,
			"hostname":     c.Hello.Hostname,
			"version":      c.Hello.Version,
			"hello_at":     c.HelloAt,
			"client_token": maskToken(c.Hello.Token),
		})
	}
	r.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"clients": out})
}

// handleCommand dispatches a synchronous command to a client and waits for the
// matching result envelope.
//
// Request body, accepted as either JSON or form-urlencoded:
//
//	{
//	  "client_id": "c-1",      // optional; defaults to first connected
//	  "action": "status",
//	  "params": { ... },
//	  "timeout": 30            // seconds, optional
//	}
func (r *relay) handleCommand(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := decodeCommandBody(req)
	if err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.Action == "" {
		http.Error(w, "action is required", http.StatusBadRequest)
		return
	}
	timeout := time.Duration(body.Timeout) * time.Second
	if timeout <= 0 {
		timeout = defaultCallTimeout
	}

	r.mu.Lock()
	c, ok := r.clients[body.ClientID]
	if !ok && body.ClientID == "" {
		for _, v := range r.clients {
			c = v
			break
		}
	}
	r.mu.Unlock()
	if c == nil {
		http.Error(w, "no client available", http.StatusServiceUnavailable)
		return
	}

	id := protocol.NewID()
	cmd := protocol.Command{ID: id, Action: body.Action, Params: body.Params}

	resCh := make(chan protocol.Result, 1)
	r.mu.Lock()
	r.pending[id] = pendingCall{ch: resCh, clientID: c.ID}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.pending, id)
		r.mu.Unlock()
	}()

	started := time.Now()
	if err := c.Send(req.Context(), cmd); err != nil {
		r.audit(map[string]any{
			"event":     "send_error",
			"client_id": c.ID,
			"action":    body.Action,
			"error":     err.Error(),
			"ts":        time.Now().UTC().Format(time.RFC3339Nano),
		})
		r.unregister(c)
		http.Error(w, "send failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	r.audit(map[string]any{
		"event":     "command",
		"client_id": c.ID,
		"action":    body.Action,
		"id":        id,
		"ts":        time.Now().UTC().Format(time.RFC3339Nano),
	})

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res := <-resCh:
		r.audit(map[string]any{
			"event":     "result",
			"client_id": c.ID,
			"action":    body.Action,
			"id":        id,
			"success":   res.Success,
			"took_ms":   res.TookMs,
			"duration_ms": time.Since(started).Milliseconds(),
			"ts":        time.Now().UTC().Format(time.RFC3339Nano),
		})
		writeJSON(w, http.StatusOK, res)
	case <-timer.C:
		r.audit(map[string]any{
			"event":     "timeout",
			"client_id": c.ID,
			"action":    body.Action,
			"id":        id,
			"ts":        time.Now().UTC().Format(time.RFC3339Nano),
		})
		http.Error(w, "timeout waiting for result", http.StatusGatewayTimeout)
	}
}

// handleHealth returns a tiny JSON liveness payload.
func (r *relay) handleHealth(w http.ResponseWriter, _ *http.Request) {
	r.mu.Lock()
	n := len(r.clients)
	r.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"clients": n,
		"ts":      time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// uploadChunkSize is the size of each chunk sent to the helper. The
// helper accepts arbitrary sizes; 64 KB matches what `gorilla/websocket`
// happily eats as a single text frame and keeps round-trips short.
const uploadChunkSize = 64 * 1024

// handleUpload streams a multipart file upload to the helper as a
// sequence of "upload_file" commands. The operator gets one HTTP
// response with the FINAL chunk's Result; intermediate chunks are
// fire-and-wait (the helper replies with bytes-written progress so we
// can detect failures early).
//
// Multipart fields:
//
//	file       — the binary to upload
//	path       — absolute destination path on the helper
//	sha256     — optional; expected lowercase hex digest
//	overwrite  — "true" to allow replacing an existing file
//	client_id  — optional; required when >1 helper is connected
//	timeout    — per-chunk timeout in seconds (default 30)
//
// Example:
//
//	curl -k -X POST \
//	  -F "file=@app.jar" \
//	  -F "path=E:\\tomcat-helper\\app.jar" \
//	  -F "sha256=$(sha256sum app.jar | cut -d' ' -f1)" \
//	  https://server:7777/upload
func (r *relay) handleUpload(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// 64 MB in memory; bigger files spill to disk. Plenty for jars.
	if err := req.ParseMultipartForm(64 << 20); err != nil {
		http.Error(w, "multipart: "+err.Error(), http.StatusBadRequest)
		return
	}

	path := req.FormValue("path")
	if path == "" {
		http.Error(w, "path required", http.StatusBadRequest)
		return
	}
	sha := req.FormValue("sha256")
	overwrite := req.FormValue("overwrite") == "true"
	cid := req.FormValue("client_id")
	timeoutSec, _ := strconv.ParseFloat(req.FormValue("timeout"), 64)
	if timeoutSec <= 0 {
		timeoutSec = 60 // 60s default, up from 30s — large files need more headroom
	}
	timeout := time.Duration(timeoutSec * float64(time.Second))

	// Stall detection: if no chunk completes within this duration, abort.
	// 5 minutes covers slow links (234 MB over 3G = ~100KB/s = ~40 min total,
	// but each 64KB chunk should finish in <60s even at 1KB/s).
	stallTimeout := 5 * time.Minute

	fileHdr, fileHeader, err := req.FormFile("file")
	if err != nil {
		http.Error(w, "file: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer fileHdr.Close()

	// Resolve client (single helper auto-pick).
	r.mu.Lock()
	if cid == "" {
		if len(r.clients) == 1 {
			for id := range r.clients {
				cid = id
			}
		} else if len(r.clients) == 0 {
			r.mu.Unlock()
			http.Error(w, "no helper connected", http.StatusServiceUnavailable)
			return
		} else {
			r.mu.Unlock()
			http.Error(w, "client_id required (multiple helpers connected)", http.StatusBadRequest)
			return
		}
	}
	client, ok := r.clients[cid]
	r.mu.Unlock()
	if !ok {
		http.Error(w, "unknown client_id: "+cid, http.StatusNotFound)
		return
	}

	uploadID := protocol.NewID()
	started := time.Now()
	r.audit(map[string]any{
		"event":     "upload_start",
		"client_id": client.ID,
		"path":      path,
		"upload_id": uploadID,
		"sha256":    sha,
		"overwrite": overwrite,
		"ts":        time.Now().UTC().Format(time.RFC3339Nano),
	})

	// Get total file size for progress reporting.
	totalBytes := fileHeader.Size
	r.uploadProgressMu.Lock()
	r.uploadProgress[uploadID] = &uploadProgressInfo{
		TotalBytes:   totalBytes,
		WrittenBytes: 0,
		Path:         path,
		ClientID:     client.ID,
		StartedAt:    started,
		LastUpdate:   started,
	}
	r.uploadProgressMu.Unlock()
	defer func() {
		r.uploadProgressMu.Lock()
		delete(r.uploadProgress, uploadID)
		r.uploadProgressMu.Unlock()
	}()

	// Stream chunks sequentially: send one Command, wait for its Result,
	// then send the next. Pipelining could speed this up significantly
	// but complicates failure handling — start simple.
	buf := make([]byte, uploadChunkSize)
	var offset int64
	var final protocol.Result
	lastProgressTime := time.Now()
	chunkCount := 0
	for {
		// Stall detection: if no chunk completed in stallTimeout, abort.
		if time.Since(lastProgressTime) > stallTimeout {
			r.audit(map[string]any{
				"event":     "upload_stall_abort",
				"client_id": client.ID,
				"upload_id": uploadID,
				"offset":    offset,
				"stalled":   time.Since(lastProgressTime).String(),
				"ts":        time.Now().UTC().Format(time.RFC3339Nano),
			})
			// Tell helper to clean up the .tmp file.
			cancelParams, _ := json.Marshal(map[string]any{"upload_id": uploadID})
			cancelCmd := protocol.Command{ID: protocol.NewID(), Action: "cancel_upload", Params: cancelParams}
			_ = client.Send(req.Context(), cancelCmd)
			http.Error(w, fmt.Sprintf("upload stalled: no progress for %s at offset %d/%d", time.Since(lastProgressTime).Round(time.Second), offset, totalBytes), http.StatusGatewayTimeout)
			return
		}
		n, readErr := io.ReadFull(fileHdr, buf)
		isLast := readErr == io.EOF || readErr == io.ErrUnexpectedEOF
		if n == 0 {
			break
		}
		if readErr != nil && !isLast {
			http.Error(w, "read upload: "+readErr.Error(), http.StatusBadRequest)
			return
		}
		chunk := make([]byte, n)
		copy(chunk, buf[:n])
		b64 := base64.StdEncoding.EncodeToString(chunk)

		params, _ := json.Marshal(map[string]any{
			"path":             path,
			"upload_id":        uploadID,
			"chunk":            b64,
			"chunk_offset":     offset,
			"chunk_done":       isLast,
			"expected_sha256":  sha,
			"overwrite":        overwrite,
		})

		id := protocol.NewID()
		resCh := make(chan protocol.Result, 1)
		r.mu.Lock()
		r.pending[id] = pendingCall{ch: resCh, clientID: client.ID}
		r.mu.Unlock()

		cmd := protocol.Command{ID: id, Action: "upload_file", Params: params}
		if err := client.Send(req.Context(), cmd); err != nil {
			r.mu.Lock()
			delete(r.pending, id)
			r.mu.Unlock()
			r.audit(map[string]any{
				"event":     "upload_send_error",
				"client_id": client.ID,
				"upload_id": uploadID,
				"offset":    offset,
				"error":     err.Error(),
				"ts":        time.Now().UTC().Format(time.RFC3339Nano),
			})
			r.unregister(client)
			http.Error(w, "send failed: "+err.Error(), http.StatusBadGateway)
			return
		}

		timer := time.NewTimer(timeout)
		select {
		case res := <-resCh:
			timer.Stop()
			r.mu.Lock()
			delete(r.pending, id)
			r.mu.Unlock()
			if !res.Success {
				r.audit(map[string]any{
					"event":     "upload_chunk_error",
					"client_id": client.ID,
					"upload_id": uploadID,
					"offset":    offset,
					"error":     res.Error,
					"ts":        time.Now().UTC().Format(time.RFC3339Nano),
				})
				// Tell helper to clean up the .tmp file.
				cancelParams, _ := json.Marshal(map[string]any{"upload_id": uploadID})
				cancelCmd := protocol.Command{ID: protocol.NewID(), Action: "cancel_upload", Params: cancelParams}
				_ = client.Send(req.Context(), cancelCmd)
				http.Error(w, "upload chunk failed: "+res.Error, http.StatusBadGateway)
				return
			}
			if isLast {
				final = res
			}
		case <-timer.C:
			r.mu.Lock()
			delete(r.pending, id)
			r.mu.Unlock()
			r.audit(map[string]any{
				"event":     "upload_timeout",
				"client_id": client.ID,
				"upload_id": uploadID,
				"offset":    offset,
				"ts":        time.Now().UTC().Format(time.RFC3339Nano),
			})
			// Tell helper to clean up the .tmp file.
			cancelParams, _ := json.Marshal(map[string]any{"upload_id": uploadID})
			cancelCmd := protocol.Command{ID: protocol.NewID(), Action: "cancel_upload", Params: cancelParams}
			_ = client.Send(req.Context(), cancelCmd)
			http.Error(w, fmt.Sprintf("timeout after %s on chunk at offset %d", timeout, offset), http.StatusGatewayTimeout)
			return
		}

		offset += int64(n)
		chunkCount++
		lastProgressTime = time.Now()
		r.uploadProgressMu.Lock()
		if info, ok := r.uploadProgress[uploadID]; ok {
			info.WrittenBytes = offset
			info.LastUpdate = lastProgressTime
		}
		r.uploadProgressMu.Unlock()
		// Log progress every 100 chunks (~6.4 MB) so operator can see movement.
		if chunkCount%100 == 0 {
			pct := float64(offset) / float64(totalBytes) * 100
			r.logger.Printf("upload %s: %d/%d bytes (%.1f%%) chunk#%d elapsed %s",
				uploadID, offset, totalBytes, pct, chunkCount, time.Since(started).Round(time.Second))
		}
		if isLast {
			break
		}
	}

	r.audit(map[string]any{
		"event":       "upload_done",
		"client_id":   client.ID,
		"upload_id":   uploadID,
		"path":        path,
		"bytes":       offset,
		"duration_ms": time.Since(started).Milliseconds(),
		"ts":          time.Now().UTC().Format(time.RFC3339Nano),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"upload_id":   uploadID,
		"path":        path,
		"bytes":       offset,
		"duration_ms": time.Since(started).Milliseconds(),
		"result":      json.RawMessage(final.Data),
	})
}

// handleUploadProgress returns the progress of an in-flight upload.
// GET /upload/{upload_id}/progress
func (r *relay) handleUploadProgress(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Extract upload_id from path: /upload/{id}/progress
	id := strings.TrimPrefix(req.URL.Path, "/upload/")
	id = strings.TrimSuffix(id, "/progress")
	if id == "" {
		http.Error(w, "upload_id required", http.StatusBadRequest)
		return
	}

	r.uploadProgressMu.Lock()
	info, ok := r.uploadProgress[id]
	r.uploadProgressMu.Unlock()
	if !ok {
		http.Error(w, "upload not found or completed", http.StatusNotFound)
		return
	}

	var percent float64
	if info.TotalBytes > 0 {
		percent = float64(info.WrittenBytes) / float64(info.TotalBytes) * 100
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"upload_id":      id,
		"path":           info.Path,
		"client_id":      info.ClientID,
		"total_bytes":    info.TotalBytes,
		"written_bytes":  info.WrittenBytes,
		"percent":        percent,
		"started_at":     info.StartedAt.UTC().Format(time.RFC3339Nano),
		"last_update":    info.LastUpdate.UTC().Format(time.RFC3339Nano),
		"elapsed_ms":     time.Since(info.StartedAt).Milliseconds(),
	})
}

// decodeCommandBody accepts application/json, application/x-www-form-urlencoded,
// or (forgivingly) any body that looks like JSON. This last branch keeps the
// common `curl -d '{"action":"status"}'` invocation working even though curl
// defaults to form-urlencoded.
func decodeCommandBody(req *http.Request) (struct {
	ClientID string          `json:"client_id"`
	Action   string          `json:"action"`
	Params   json.RawMessage `json:"params"`
	Timeout  float64         `json:"timeout"`
}, error) {
	var body struct {
		ClientID string          `json:"client_id"`
		Action   string          `json:"action"`
		Params   json.RawMessage `json:"params"`
		Timeout  float64         `json:"timeout"`
	}
	raw, _ := io.ReadAll(req.Body)
	ct := req.Header.Get("Content-Type")
	if i := strings.Index(ct, ";"); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(strings.ToLower(ct))
	// Sniff first: a body that looks like JSON is always treated as JSON,
	// regardless of content-type. This keeps `curl -d '{"action":"x"}'`
	// working despite curl's default form-urlencoded content-type.
	switch {
	case isLikelyJSON(raw):
		if err := json.Unmarshal(raw, &body); err != nil {
			return body, err
		}
	case ct == "application/x-www-form-urlencoded" || (ct == "" && !isLikelyJSON(raw)):
		values, err := url.ParseQuery(string(raw))
		if err != nil {
			return body, err
		}
		body.Action = values.Get("action")
		body.ClientID = values.Get("client_id")
		if v := values.Get("timeout"); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				body.Timeout = f
			}
		}
		if raw := values.Get("params"); raw != "" {
			body.Params = json.RawMessage(raw)
		}
	case ct == "":
		// Empty body, nothing to decode.
		return body, nil
	default:
		return body, fmt.Errorf("unsupported content-type: %s", ct)
	}
	return body, nil
}

func isLikelyJSON(b []byte) bool {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\r', '\n':
			continue
		case '{', '[':
			return true
		default:
			return false
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func maskToken(t string) string {
	if len(t) <= 4 {
		return "****"
	}
	return strings.Repeat("*", len(t)-4) + t[len(t)-4:]
}
