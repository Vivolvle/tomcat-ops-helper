// Package dbop implements the persistent database connector used by the
// helper's esb_db_operator / erp_db_operator tools.
//
// Two layers sit on top of each other:
//
//   - Op is the small interface every connector satisfies (Status /
//     Query / Exec / Close). The action layer talks to it via this
//     interface so unit tests can swap a FakeOp in without touching a
//     real database.
//
//   - SQLOp is the production implementation, wrapping *sql.DB. It is
//     the only thing that knows about drivers. Three are wired in
//     (mysql / sqlserver / oracle); the active one is chosen by the
//     connector's config.json block.
//
// The package also owns the SQL script splitter (SplitScript), the
// Query / Exec router (routeStatement), and the high-level RunScript
// helper that turns a multi-statement script into the structured
// per-statement result the MCP tool returns.
package dbop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MaxRowsCap is the hard ceiling for a single Query's row count. The
// action layer also caps by the operator's request (max_rows argument);
// this constant is the absolute upper bound, matching the existing
// tail / list_logs discipline in this repo.
const MaxRowsCap = 5000

// DefaultMaxRows is what the action picks when the caller omits max_rows.
const DefaultMaxRows = 1000

// Op is the contract every connector satisfies. Implementations must be
// safe for concurrent use; SQLOp achieves this by virtue of *sql.DB
// being goroutine-safe and the retry wrapper being stateless.
type Op interface {
	// Status reports the connector's current health. Cheap; safe to
	// call any time. Used by the action's "not configured" check and
	// the audit log.
	Status(ctx context.Context) Status

	// Query runs one SELECT-like statement and returns up to maxRows
	// rows. Negative or zero maxRows means "use DefaultMaxRows";
	// values above MaxRowsCap are clamped down.
	Query(ctx context.Context, sql string, maxRows int) (QueryResult, error)

	// Exec runs one non-SELECT statement and returns the affected row
	// count. SQL like INSERT / UPDATE / DELETE / DDL all go here.
	Exec(ctx context.Context, sql string) (ExecResult, error)

	// Close releases the underlying connection pool. Idempotent.
	Close() error
}

// Status is the JSON-serialisable health snapshot returned by Op.Status
// and embedded in RunScriptResult. Open=false means the connector
// failed to (re)open; LastError carries the most recent failure reason
// and LastErrorAt its timestamp.
type Status struct {
	Configured    bool       `json:"configured"`
	Open          bool       `json:"open"`
	Driver        string     `json:"driver,omitempty"`
	Host          string     `json:"host,omitempty"`
	Port          int        `json:"port,omitempty"`
	Database      string     `json:"database,omitempty"`
	CurrentSchema string     `json:"current_schema,omitempty"`
	LatencyMs     int64      `json:"latency_ms,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	LastErrorAt   *time.Time `json:"last_error_at,omitempty"`
}

// QueryColumn is one column of a Query result.
type QueryColumn struct {
	Name     string `json:"name"`
	Type     string `json:"type,omitempty"` // driver-reported DB type
	Nullable bool   `json:"nullable,omitempty"`
}

// QueryResult is the payload of a single SELECT-style statement.
type QueryResult struct {
	Columns   []QueryColumn `json:"columns"`
	Rows      [][]any       `json:"rows"`
	RowCount  int           `json:"row_count"`
	Truncated bool          `json:"truncated,omitempty"`
	TookMs    int64         `json:"took_ms"`
}

// ExecResult is the payload of a single non-SELECT statement.
type ExecResult struct {
	AffectedRows int64  `json:"affected_rows"`
	LastInsertID string `json:"last_insert_id,omitempty"` // MySQL LAST_INSERT_ID() if non-zero
	TookMs       int64  `json:"took_ms"`
}

// StatementKind tells RunScript (and tests) whether a parsed statement
// should be sent through Query or Exec.
type StatementKind int

const (
	KindUnknown StatementKind = iota
	KindQuery
	KindExec
)

// StatementResult is one entry in RunScriptResult.Statements. The
// union shape mirrors what Navicat shows: queries get columns + rows,
// exec gets affected_rows.
type StatementResult struct {
	Index        int           `json:"index"`
	Kind         StatementKind `json:"-"`
	Type         string        `json:"type"` // "query" or "exec"
	SQL          string        `json:"sql,omitempty"`
	Columns      []QueryColumn `json:"columns,omitempty"`
	Rows         [][]any       `json:"rows,omitempty"`
	RowCount     int           `json:"row_count,omitempty"`
	Truncated    bool          `json:"truncated,omitempty"`
	AffectedRows int64         `json:"affected_rows,omitempty"`
	LastInsertID string        `json:"last_insert_id,omitempty"`
	TookMs       int64         `json:"took_ms"`
	Error        string        `json:"error,omitempty"`
}

// RunScriptResult is the JSON the action returns to the operator.
type RunScriptResult struct {
	Driver        string            `json:"driver"`
	Database      string            `json:"database,omitempty"`
	CurrentSchema string            `json:"current_schema,omitempty"`
	Statements    []StatementResult `json:"statements"`
	TotalTookMs   int64             `json:"total_took_ms"`
	Truncated     bool              `json:"truncated,omitempty"`
	FirstError    string            `json:"first_error,omitempty"`
	FirstErrorAt  int               `json:"first_error_at,omitempty"` // index of first failed statement
}

// RunScript splits script into statements, dispatches each via Query or
// Exec based on its leading keyword, and aggregates the per-statement
// results. maxRows applies to every Query (clamped to [1, MaxRowsCap]).
// perStatementTimeout bounds each statement; totalTimeout bounds the
// whole script. Zero values fall back to defaults (DefaultMaxRows / 30s
// per statement / 120s total).
//
// RunScript stops at the first statement that errors and returns the
// partial result with FirstError set. This matches the "stop on error"
// default that Navicat ships with; the user can always re-issue the
// remaining statements.
func RunScript(ctx context.Context, op Op, script string, maxRows int, perStatementTimeout, totalTimeout time.Duration) RunScriptResult {
	if totalTimeout <= 0 {
		totalTimeout = 120 * time.Second
	}
	if perStatementTimeout <= 0 {
		perStatementTimeout = 30 * time.Second
	}
	if maxRows <= 0 {
		maxRows = DefaultMaxRows
	}
	if maxRows > MaxRowsCap {
		maxRows = MaxRowsCap
	}

	scriptCtx, cancel := context.WithTimeout(ctx, totalTimeout)
	defer cancel()

	started := time.Now()
	res := RunScriptResult{}
	if st, ok := op.(interface{ Status(context.Context) Status }); ok {
		s := st.Status(scriptCtx)
		res.Driver = s.Driver
		res.Database = s.Database
		res.CurrentSchema = s.CurrentSchema
	}

	stmts := SplitScriptForDriver(script, res.Driver)
	for i, raw := range stmts {
		if scriptCtx.Err() != nil {
			sr := StatementResult{Index: i, SQL: raw}
			sr.Error = fmt.Sprintf("script aborted: %v", scriptCtx.Err())
			res.Statements = append(res.Statements, sr)
			if res.FirstError == "" {
				res.FirstError = sr.Error
				res.FirstErrorAt = i
			}
			break
		}
		stmt := trimStatement(raw)
		if stmt == "" {
			// Preserve indexing for callers that key by index.
			res.Statements = append(res.Statements, StatementResult{Index: i})
			continue
		}
		sr := StatementResult{Index: i, SQL: stmt}
		kind := RouteStatement(stmt)
		sr.Kind = kind
		stmtCtx, stmtCancel := context.WithTimeout(scriptCtx, perStatementTimeout)
		switch kind {
		case KindQuery:
			sr.Type = "query"
			qr, err := op.Query(stmtCtx, stmt, maxRows)
			if err != nil {
				sr.Error = err.Error()
			} else {
				sr.Columns = qr.Columns
				sr.Rows = qr.Rows
				// Defensive: many Op implementations forget to set
				// RowCount. We compute it from len(Rows) so callers
				// always see the truth.
				sr.RowCount = qr.RowCount
				if sr.RowCount == 0 && len(qr.Rows) > 0 {
					sr.RowCount = len(qr.Rows)
				}
				sr.Truncated = qr.Truncated
				if qr.Truncated {
					res.Truncated = true
				}
			}
			sr.TookMs = qr.TookMs
		default: // KindExec (also covers KindUnknown — safer to treat as exec)
			sr.Type = "exec"
			er, err := op.Exec(stmtCtx, stmt)
			if err != nil {
				sr.Error = err.Error()
			} else {
				sr.AffectedRows = er.AffectedRows
				sr.LastInsertID = er.LastInsertID
			}
			sr.TookMs = er.TookMs
		}
		stmtCancel()
		res.Statements = append(res.Statements, sr)
		if sr.Error != "" {
			if res.FirstError == "" {
				res.FirstError = sr.Error
				res.FirstErrorAt = i
			}
			break
		}
	}
	res.TotalTookMs = time.Since(started).Milliseconds()
	return res
}

// ErrNotConfigured is returned by Status when the connector was never
// built (the config block was missing or invalid at startup).
var ErrNotConfigured = errors.New("db connector not configured")

// TrimStatement trims whitespace and leading comments from a raw SQL
// fragment returned by SplitScript. Exposed for tests that want to
// assert on the cleaned form.
func TrimStatement(s string) string { return trimStatement(s) }

func trimStatement(s string) string {
	s = stripLeadingComments(s)
	// Collapse interior whitespace.
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r') {
		s = s[1:]
	}
	// NUL sentinel emitted by SplitScriptForDriver for Oracle PL/SQL
	// blocks: strip it, then trim trailing whitespace only, leaving
	// the block's closing ';' intact ("END;" must not become "END").
	// See problem.md 2026-09-21.
	if strings.HasSuffix(s, "\x00") {
		s = s[:len(s)-1]
		for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t' || s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
			s = s[:len(s)-1]
		}
		return s
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t' || s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ';') {
		s = s[:len(s)-1]
	}
	return s
}

// stripLeadingComments peels off any sequence of `-- …` / `/* … */`
// comments and whitespace from the front of s.
func stripLeadingComments(s string) string {
	for {
		// Skip whitespace.
		i := 0
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
			i++
		}
		s = s[i:]
		if len(s) == 0 {
			return s
		}
		// `-- line comment`
		if len(s) >= 2 && s[0] == '-' && s[1] == '-' {
			j := 2
			for j < len(s) && s[j] != '\n' {
				j++
			}
			s = s[j:]
			continue
		}
		// `/* block comment */`
		if len(s) >= 2 && s[0] == '/' && s[1] == '*' {
			j := 2
			for j < len(s)-1 && !(s[j] == '*' && s[j+1] == '/') {
				j++
			}
			if j >= len(s)-1 {
				// unterminated; bail out
				return s
			}
			s = s[j+2:]
			continue
		}
		return s
	}
}
