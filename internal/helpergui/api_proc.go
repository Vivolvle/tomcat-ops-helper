package helpergui

import (
	"net/http"
	"strconv"
	"time"
)

// handleProcStatus backs the status lamp (polled every ~2s).
func (s *Server) handleProcStatus(w http.ResponseWriter, r *http.Request) {
	st := s.proc.Status()
	writeJSON(w, http.StatusOK, st)
}

// handleProcStart launches helper.exe under this GUI session.
func (s *Server) handleProcStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	pid, err := s.proc.Start()
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pid": pid})
}

// handleProcStop tree-kills the helper started by this GUI.
func (s *Server) handleProcStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	code, err := s.proc.Stop(15 * time.Second)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "exit_code": code})
}

// handleProcLogs streams ring lines since a cursor. Poll 1s while the
// helper runs under the GUI; `wrapped` means lines were evicted behind
// the cursor and the client should resync to `next`.
func (s *Server) handleProcLogs(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
	lines, next, oldest, wrapped := s.proc.Ring().Since(since, 500)
	st := s.proc.Status()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"source":   st.Source,
		"running":  st.Running,
		"lines":    lines,
		"next":     next,
		"oldest":   oldest,
		"wrapped":  wrapped,
	})
}

// handleLogTail tails helper.log (audit JSONL) by byte offset — the log
// source when the helper was NOT started by this GUI. offset=0 on the
// first call starts near the end of the file (recent history), then the
// client keeps passing the returned offset back.
func (s *Server) handleLogTail(w http.ResponseWriter, r *http.Request) {
	off, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	path := s.proc.LogPath()
	lines, newOffset, err := TailFile(path, off, 200)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "path": path, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": path, "offset": newOffset, "lines": lines})
}
