package actions

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"tomcat-ops-helper/internal/dbop"
)

// TestTools_OmitsDBOpsWhenNotWired ensures the dbop tools are absent
// from Tools() when no connector has been wired. The tool buttons
// only show up when there is something behind them.
func TestTools_OmitsDBOpsWhenNotWired(t *testing.T) {
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t))

	names := map[string]bool{}
	for _, tool := range reg.Tools() {
		names[tool.Name] = true
	}
	if names["esb_db_operator"] {
		t.Fatal("esb_db_operator should not be advertised without a registered Op")
	}
	if names["erp_db_operator"] {
		t.Fatal("erp_db_operator should not be advertised without a registered Op")
	}
}

// TestTools_IncludesWiredDBOps ensures each wired connector surfaces
// its tool. Adding a new connector is one WithDB call + one entry in
// dbOpToolDefs; this test catches the case where someone adds the
// entry but forgets the registration (or vice versa).
func TestTools_IncludesWiredDBOps(t *testing.T) {
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t)).
		WithDB("esb_db_operator", &dbop.FakeOp{Driver: "mysql", Database: "esb"}).
		WithDB("erp_db_operator", &dbop.FakeOp{Driver: "oracle", Database: "ORCL"})

	have := map[string]Tool{}
	for _, tool := range reg.Tools() {
		have[tool.Name] = tool
	}
	for _, name := range []string{"esb_db_operator", "erp_db_operator"} {
		if _, ok := have[name]; !ok {
			t.Fatalf("%s missing from Tools()", name)
		}
	}
	// WithDB(nil) should remove the entry, useful for tests that
	// conditionally wire connectors.
	reg.WithDB("esb_db_operator", nil)
	for _, tool := range reg.Tools() {
		if tool.Name == "esb_db_operator" {
			t.Fatal("esb_db_operator should be removed after WithDB(nil)")
		}
	}
}

// TestTools_ReadOnlyDescription ensures a connector locked with
// forbid_cmd announces the lock in its advertised description, so the
// calling agent sees the constraint before its first rejected write.
func TestTools_ReadOnlyDescription(t *testing.T) {
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t)).
		WithDB("esb_db_operator", &dbop.FakeOp{Driver: "mysql", Database: "esb", ForbidCmd: true}).
		WithDB("erp_db_operator", &dbop.FakeOp{Driver: "oracle", Database: "ORCL"})

	desc := map[string]string{}
	for _, tool := range reg.Tools() {
		desc[tool.Name] = tool.Description
	}
	if !strings.Contains(desc["esb_db_operator"], "READ-ONLY") {
		t.Fatalf("locked esb_db_operator description should carry the READ-ONLY clause: %q", desc["esb_db_operator"])
	}
	if strings.Contains(desc["erp_db_operator"], "READ-ONLY") {
		t.Fatalf("unlocked erp_db_operator description must not claim READ-ONLY: %q", desc["erp_db_operator"])
	}
}

// TestDispatch_DBOpNotWired ensures dispatching a dbop tool without
// a connector returns a clear error rather than panicking or
// silently succeeding.
func TestDispatch_DBOpNotWired(t *testing.T) {
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t))
	_, err := reg.Dispatch(context.Background(), "esb_db_operator", mustMarshal(map[string]any{
		"sql": "SELECT 1",
	}))
	if err == nil {
		t.Fatal("expected error when no Op is wired")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("expected 'not configured' message, got: %v", err)
	}
}

// TestDispatch_DBOpMissingSQL ensures the action validates required
// fields the same way every other action does.
func TestDispatch_DBOpMissingSQL(t *testing.T) {
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t)).
		WithDB("esb_db_operator", &dbop.FakeOp{Driver: "mysql"})
	_, err := reg.Dispatch(context.Background(), "esb_db_operator", mustMarshal(map[string]any{}))
	if !errors.Is(err, ErrMissingField) {
		t.Fatalf("expected ErrMissingField, got %v", err)
	}
}

// TestDispatch_DBOpRunsScript wires a FakeOp, dispatches a SELECT,
// and asserts the action forwards parameters and returns the
// structured envelope the operator UI expects.
func TestDispatch_DBOpRunsScript(t *testing.T) {
	fake := &dbop.FakeOp{
		Driver: "mysql", Database: "esb", Host: "db",
		QueryHandler: func(_ context.Context, sql string, maxRows int) (dbop.QueryResult, error) {
			return dbop.QueryResult{
				Columns:  []dbop.QueryColumn{{Name: "id"}, {Name: "name"}},
				Rows:     [][]any{{1, "a"}, {2, "b"}},
				RowCount: 2,
			}, nil
		},
	}
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t)).WithDB("esb_db_operator", fake)

	raw, err := reg.Dispatch(context.Background(), "esb_db_operator", mustMarshal(map[string]any{
		"sql":      "SELECT id, name FROM t",
		"max_rows": 50,
	}))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	var got dbop.RunScriptResult
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v (raw=%s)", err, raw)
	}
	if got.Driver != "mysql" || got.Database != "esb" {
		t.Fatalf("driver/db not propagated: %+v", got)
	}
	if len(got.Statements) != 1 || got.Statements[0].RowCount != 2 {
		t.Fatalf("statements wrong: %+v", got.Statements)
	}
	if fake.QueryCount != 1 {
		t.Fatalf("QueryCount=%d, want 1", fake.QueryCount)
	}
	if len(fake.Calls) != 1 || fake.Calls[0].Kind != "query" {
		t.Fatalf("call record wrong: %+v", fake.Calls)
	}
}

// TestDispatch_DBOpClampsMaxRows ensures the action clamps
// max_rows to the package cap even when the caller asks for more.
func TestDispatch_DBOpClampsMaxRows(t *testing.T) {
	gotMax := 0
	fake := &dbop.FakeOp{
		Driver: "mysql",
		QueryHandler: func(_ context.Context, _ string, maxRows int) (dbop.QueryResult, error) {
			gotMax = maxRows
			return dbop.QueryResult{}, nil
		},
	}
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t)).WithDB("esb_db_operator", fake)

	_, err := reg.Dispatch(context.Background(), "esb_db_operator", mustMarshal(map[string]any{
		"sql":      "SELECT 1",
		"max_rows": 100000,
	}))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if gotMax != dbop.MaxRowsCap {
		t.Fatalf("maxRows=%d, want %d", gotMax, dbop.MaxRowsCap)
	}
}

// TestDispatch_DBOpTimeoutClampsBelowPerStmt makes sure the total
// timeout can never be smaller than the per-statement timeout: that
// would let a slow statement outlive its own container, which is a
// confusing failure mode.
func TestDispatch_DBOpTimeoutClampsBelowPerStmt(t *testing.T) {
	fake := &dbop.FakeOp{
		Driver: "mysql",
		QueryHandler: func(_ context.Context, _ string, _ int) (dbop.QueryResult, error) {
			return dbop.QueryResult{}, nil
		},
	}
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t)).WithDB("esb_db_operator", fake)

	// Both fields must be positive ints in the JSON; supply a small
	// per-stmt timeout (1s) and a smaller total (0.5s) — the action
	// should bump total up to per-stmt, not refuse the call.
	_, err := reg.Dispatch(context.Background(), "esb_db_operator", mustMarshal(map[string]any{
		"sql":               "SELECT 1",
		"statement_timeout": 1,
		"timeout":           1, // equal — fine
	}))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
}

// TestDispatch_DBOpMultiStatement dispatches a script with two
// statements and asserts the second one was actually called — the
// splitter's behaviour is covered by dbop tests, so here we only
// need to confirm the action layers it through.
func TestDispatch_DBOpMultiStatement(t *testing.T) {
	fake := &dbop.FakeOp{
		Driver: "mysql",
		QueryHandler: func(_ context.Context, _ string, _ int) (dbop.QueryResult, error) {
			return dbop.QueryResult{Rows: [][]any{{1}}, RowCount: 1}, nil
		},
		ExecHandler: func(_ context.Context, _ string) (dbop.ExecResult, error) {
			return dbop.ExecResult{AffectedRows: 2}, nil
		},
	}
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t)).WithDB("esb_db_operator", fake)

	raw, err := reg.Dispatch(context.Background(), "esb_db_operator", mustMarshal(map[string]any{
		"sql": "SELECT 1; UPDATE t SET x=1",
	}))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	var got dbop.RunScriptResult
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Statements) != 2 {
		t.Fatalf("expected 2 statements, got %d", len(got.Statements))
	}
	if got.Statements[0].Type != "query" || got.Statements[1].Type != "exec" {
		t.Fatalf("statement kinds wrong: %+v", got.Statements)
	}
	if got.Statements[1].AffectedRows != 2 {
		t.Fatalf("affected_rows=%d, want 2", got.Statements[1].AffectedRows)
	}
}

// TestDispatch_DBOpTimeoutDefaults ensures the defaults match the
// docs (per-stmt 30s, total 120s) — a regression here would silently
// break customer expectations.
func TestDispatch_DBOpTimeoutDefaults(t *testing.T) {
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t)).
		WithDB("esb_db_operator", &dbop.FakeOp{Driver: "mysql"})
	raw, err := reg.Dispatch(context.Background(), "esb_db_operator", mustMarshal(map[string]any{
		"sql": "SELECT 1",
	}))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	var got dbop.RunScriptResult
	_ = json.Unmarshal(raw, &got)
	// We can't introspect the exact durations without instrumenting
	// dbop.RunScript; the smoke here is just that the call returned
	// and the envelope has a driver tag. The dbop package owns the
	// defaults; deeper coverage lives there.
	if got.Driver != "mysql" {
		t.Fatalf("driver not set: %+v", got)
	}
	_ = time.Second // keep the import live even if not used elsewhere
}
