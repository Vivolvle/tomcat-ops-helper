// Package helpergui serves the customer-facing local web UI: a visual
// config.json editor with connection tests, an helper.exe launcher, and
// a live log pane. It binds to 127.0.0.1 only and is embedded in
// helper-gui.exe; see cmd/helper-gui.
package helpergui

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// guiHeader must accompany every mutating /api request. A custom header
// forces a CORS preflight on cross-origin requests, which this server
// never answers — so a malicious web page in the customer's browser
// cannot drive the local API (classic local-CSRF guard).
const guiHeader = "X-Requested-With"

// DefaultPort is the preferred local UI port; a busy port falls back to
// a random free one (the chosen URL is printed to the console and shown
// in the page).
const DefaultPort = 7788

// Server hosts the GUI HTTP API.
type Server struct {
	cfgPath    string
	helperPath string
	store      *ConfigStore
	proc       *ProcManager
}

// New builds a Server serving config at cfgPath and launching the helper
// binary at helperPath.
func New(cfgPath, helperPath string) *Server {
	return &Server{
		cfgPath:    cfgPath,
		helperPath: helperPath,
		store:      &ConfigStore{Path: cfgPath},
		proc:       NewProcManager(helperPath, cfgPath),
	}
}

// Handler returns the full http handler (embedded UI + /api routes).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/config", s.requireGUIHeader(s.handleConfig))
	mux.HandleFunc("/api/fs/dirs", s.requireGUIHeader(s.handleFsDirs))
	mux.HandleFunc("/api/test/db", s.requireGUIHeader(s.handleTestDB))
	mux.HandleFunc("/api/test/server", s.requireGUIHeader(s.handleTestServer))
	mux.HandleFunc("/api/proc/status", s.requireGUIHeader(s.handleProcStatus))
	mux.HandleFunc("/api/proc/start", s.requireGUIHeader(s.handleProcStart))
	mux.HandleFunc("/api/proc/stop", s.requireGUIHeader(s.handleProcStop))
	mux.HandleFunc("/api/proc/logs", s.requireGUIHeader(s.handleProcLogs))
	mux.HandleFunc("/api/logs/tail", s.requireGUIHeader(s.handleLogTail))
	return mux
}

// Shutdown stops the helper child (signal-handler path).
func (s *Server) Shutdown() { s.proc.Shutdown() }

// requireGUIHeader rejects requests missing the helper-gui marker header.
func (s *Server) requireGUIHeader(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(guiHeader) != "helper-gui" {
			writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "missing helper-gui request header"})
			return
		}
		next(w, r)
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

// handleConfig serves the current config document (unknown fields
// included, passwords in cleartext — same trust level as the file on
// disk, and the bind is loopback-only) and accepts overlay saves.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		doc, exists, err := s.store.LoadDoc()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if !exists {
			doc = defaultDoc()
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":            true,
			"path":          s.cfgPath,
			"exists":        exists,
			"helper_path":   s.helperPath,
			"helper_exists": fileExists(s.helperPath),
			"doc":           doc,
		})
	case http.MethodPost:
		var body struct {
			Fields map[string]any `json:"fields"`
		}
		if err := decodeBody(r, &body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if len(body.Fields) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "fields is required"})
			return
		}
		restartRequired, err := s.store.Save(body.Fields)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restart_required": restartRequired})
	default:
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
	}
}

// handleFsDirs backs the folder picker: directories only, drive roots
// when path is empty. Localhost-only by bind; no file contents leak.
func (s *Server) handleFsDirs(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		drives, err := ListDrives()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		entries := make([]DirEntryInfo, 0, len(drives))
		for _, d := range drives {
			entries = append(entries, DirEntryInfo{Name: d, Path: d})
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": "", "parent": nil, "entries": entries})
		return
	}
	parent, entries, err := ListDirs(path)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	var parentAny any
	if parent != "" {
		parentAny = parent
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": path, "parent": parentAny, "entries": entries})
}

// Listen binds 127.0.0.1 on the preferred port, falling back to a
// random free port when it is taken.
func Listen(preferredPort int) (net.Listener, error) {
	if l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", preferredPort)); err == nil {
		return l, nil
	}
	return net.Listen("tcp", "127.0.0.1:0")
}

// OpenBrowser asks Windows to open the URL in the default browser.
func OpenBrowser(url string) error {
	return runOpenBrowser(url)
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func dirExistsAny(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<20))
	return dec.Decode(v)
}

// absNextTo returns path resolved against baseDir when relative.
func absNextTo(baseDir, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	abs, err := filepath.Abs(filepath.Join(baseDir, path))
	if err != nil {
		return path
	}
	return abs
}
