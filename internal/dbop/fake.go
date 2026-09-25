package dbop

import (
	"context"
	"sync"
	"time"
)

// FakeOp is an Op that does not touch any database. Tests seed the
// per-statement handler and assert on the calls.
//
// The zero value is ready to use; QueryHandler / ExecHandler default
// to "return error". Set Driver / Database to populate the JSON
// status / result envelopes.
type FakeOp struct {
	mu sync.Mutex

	Driver        string
	Database      string
	CurrentSchema string
	Host          string

	// Open toggles Status.Open. Defaults to true.
	Open bool

	// ForbidCmd makes the fake advertise itself read-only (ReadOnly()
	// and Status.ReadOnly), mirroring SQLOp's forbid_cmd lock. The fake
	// does NOT enforce the policy — that lives in SQLOp; this only
	// feeds the tool-description builder.
	ForbidCmd bool

	// LastError is surfaced by Status when Open is false.
	LastError string

	// QueryHandler is called by Query. Receives the SQL text and the
	// maxRows the action asked for. Return a QueryResult or error.
	QueryHandler func(ctx context.Context, sql string, maxRows int) (QueryResult, error)

	// ExecHandler is called by Exec.
	ExecHandler func(ctx context.Context, sql string) (ExecResult, error)

	// Calls records every (kind, sql) pair for assertions.
	Calls []FakeCall

	// QueryCount / ExecCount are convenience counters.
	QueryCount int
	ExecCount  int
}

// FakeCall records one statement the action layer dispatched.
type FakeCall struct {
	Kind string // "query" or "exec"
	SQL  string
}

// ReadOnly satisfies the same optional accessor SQLOp exposes; see
// SQLOp.ReadOnly.
func (f *FakeOp) ReadOnly() bool { return f.ForbidCmd }

// Status satisfies Op.
func (f *FakeOp) Status(ctx context.Context) Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := Status{
		Configured:    true,
		Open:          f.Open,
		Driver:        f.Driver,
		Host:          f.Host,
		Database:      f.Database,
		CurrentSchema: f.CurrentSchema,
		ReadOnly:      f.ForbidCmd,
	}
	if f.LastError != "" {
		st.LastError = f.LastError
		now := time.Now().UTC()
		st.LastErrorAt = &now
	}
	return st
}

// Query satisfies Op.
func (f *FakeOp) Query(ctx context.Context, sql string, maxRows int) (QueryResult, error) {
	f.mu.Lock()
	f.QueryCount++
	f.Calls = append(f.Calls, FakeCall{Kind: "query", SQL: sql})
	h := f.QueryHandler
	f.mu.Unlock()
	if h == nil {
		return QueryResult{}, errFakeNoHandler
	}
	return h(ctx, sql, maxRows)
}

// Exec satisfies Op.
func (f *FakeOp) Exec(ctx context.Context, sql string) (ExecResult, error) {
	f.mu.Lock()
	f.ExecCount++
	f.Calls = append(f.Calls, FakeCall{Kind: "exec", SQL: sql})
	h := f.ExecHandler
	f.mu.Unlock()
	if h == nil {
		return ExecResult{}, errFakeNoHandler
	}
	return h(ctx, sql)
}

// Close satisfies Op. Always nil for the fake.
func (f *FakeOp) Close() error { return nil }

// errFakeNoHandler is the "you forgot to seed the handler" sentinel.
var errFakeNoHandler = &fakeErr{msg: "FakeOp: no handler seeded"}

// fakeErr lets tests assert via errors.Is without leaking a sentinel.
type fakeErr struct{ msg string }

func (e *fakeErr) Error() string { return e.msg }
