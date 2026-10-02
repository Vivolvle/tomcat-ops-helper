package helpergui

import (
	"context"
	"net/http"
	"time"

	"tomcat-ops-helper/internal/config"
	"tomcat-ops-helper/internal/dbop"
)

// DBTestResult is the reply of POST /api/test/db.
type DBTestResult struct {
	OK        bool   `json:"ok"`
	Driver    string `json:"driver"`
	LatencyMs int64  `json:"latency_ms"`
	// ErrorKind: "" on success, "validate" for a structurally incomplete
	// block, "connect" when the driver dial / ping failed.
	ErrorKind string `json:"error_kind"`
	Error     string `json:"error,omitempty"`
}

// TestDB validates cfg and, when it parses, opens a real connection via
// dbop.NewSQLOp — the exact DSN construction the helper itself uses
// (including the mysqlDSNEscape password handling), so a green test here
// means the helper will connect too.
func TestDB(ctx context.Context, cfg *config.DBConfig) DBTestResult {
	res := DBTestResult{Driver: cfg.Driver()}
	if err := cfg.Validate(); err != nil {
		res.ErrorKind = "validate"
		res.Error = err.Error()
		return res
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	started := time.Now()
	op, err := dbop.NewSQLOp(pingCtx, cfg)
	res.LatencyMs = time.Since(started).Milliseconds()
	// NewSQLOp returns a non-nil Op even when the eager ping failed
	// (retry-once machinery inside); always close to release the pool.
	if op != nil {
		_ = op.Close()
	}
	if err != nil {
		res.ErrorKind = "connect"
		res.Error = err.Error()
		return res
	}
	res.OK = true
	return res
}

// handleTestDB runs one "测试连接" click for the esb or erp block.
// The frontend sends the form's current values, not the saved file —
// you can test before saving.
func (s *Server) handleTestDB(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Block string          `json:"block"` // "esb" | "erp"
		Cfg   *config.DBConfig `json:"cfg"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if body.Block != "esb" && body.Block != "erp" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": `block must be "esb" or "erp"`})
		return
	}
	if body.Cfg == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "cfg is required"})
		return
	}
	// ESB is implicitly mysql; ERP keeps its explicit type.
	if body.Block == "esb" {
		body.Cfg.Type = ""
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, TestDB(ctx, body.Cfg))
}
