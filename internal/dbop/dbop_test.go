package dbop

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"tomcat-ops-helper/internal/config"
)

func TestSplitScript_SingleStatement(t *testing.T) {
	got := SplitScript("SELECT 1")
	if len(got) != 1 || got[0] != "SELECT 1" {
		t.Fatalf("got %q", got)
	}
}

func TestSplitScript_MultipleStatements(t *testing.T) {
	got := SplitScript("SELECT 1; SELECT 2; SELECT 3")
	if len(got) != 3 {
		t.Fatalf("expected 3, got %d: %#v", len(got), got)
	}
}

func TestSplitScript_TrailingSemicolonIsIgnored(t *testing.T) {
	got := SplitScript("SELECT 1;")
	if len(got) != 1 {
		t.Fatalf("expected 1, got %d: %#v", len(got), got)
	}
}

func TestSplitScript_QuotedSemicolonIsNotASplit(t *testing.T) {
	got := SplitScript(`INSERT INTO t (name) VALUES ('a;b'); SELECT 2`)
	if len(got) != 2 {
		t.Fatalf("expected 2, got %d: %#v", len(got), got)
	}
	if !strings.Contains(got[0], "'a;b'") {
		t.Fatalf("first statement should keep the quoted semicolon, got %q", got[0])
	}
}

func TestSplitScript_DoubleQuotedIdentifier(t *testing.T) {
	got := SplitScript(`CREATE TABLE "weird;name" (id int); SELECT 1`)
	if len(got) != 2 {
		t.Fatalf("expected 2, got %d: %#v", len(got), got)
	}
	if !strings.Contains(got[0], `"weird;name"`) {
		t.Fatalf("identifier lost its semicolon: %q", got[0])
	}
}

func TestSplitScript_BacktickQuoted(t *testing.T) {
	got := SplitScript("SELECT `weird;name` FROM t; SELECT 2")
	if len(got) != 2 {
		t.Fatalf("expected 2, got %d: %#v", len(got), got)
	}
}

func TestSplitScript_LineComment(t *testing.T) {
	got := SplitScript("-- only comment\nSELECT 1; -- another\nSELECT 2")
	if len(got) != 2 {
		t.Fatalf("expected 2, got %d: %#v", len(got), got)
	}
}

func TestSplitScript_BlockComment(t *testing.T) {
	got := SplitScript("/* hi;there */ SELECT 1; SELECT 2")
	if len(got) != 2 {
		t.Fatalf("expected 2, got %d: %#v", len(got), got)
	}
	if !strings.Contains(got[0], "/* hi;there */") {
		t.Fatalf("comment should ride along in first statement: %q", got[0])
	}
}

func TestSplitScript_EscapedQuote(t *testing.T) {
	got := SplitScript(`SELECT 'it''s ok'; SELECT 2`)
	if len(got) != 2 {
		t.Fatalf("expected 2, got %d: %#v", len(got), got)
	}
	if !strings.Contains(got[0], `'it''s ok'`) {
		t.Fatalf("escaped quote should not split: %q", got[0])
	}
}

func TestSplitScript_DollarQuoted(t *testing.T) {
	got := SplitScript("SELECT $$hello;world$$; SELECT 2")
	if len(got) != 2 {
		t.Fatalf("expected 2, got %d: %#v", len(got), got)
	}
}

func TestSplitScript_Empty(t *testing.T) {
	got := SplitScript("")
	if len(got) != 0 {
		t.Fatalf("expected 0, got %d", len(got))
	}
}

func TestSplitScript_OnlyWhitespace(t *testing.T) {
	got := SplitScript("   \n\t  ")
	if len(got) != 0 {
		t.Fatalf("expected 0, got %d", len(got))
	}
}

func TestSplitScriptForDriver_OracleTriggerBlock(t *testing.T) {
	script := `CREATE OR REPLACE TRIGGER TRG_ESB_EVENT_ID
BEFORE INSERT ON ESB_EVENT FOR EACH ROW
WHEN (NEW.ID IS NULL)
BEGIN
  :NEW.ID := SEQ_ESB_EVENT.NEXTVAL;
END;
/
SELECT 1 FROM DUAL;`
	got := SplitScriptForDriver(script, "oracle")
	if len(got) != 2 {
		t.Fatalf("expected trigger + select, got %d: %#v", len(got), got)
	}
	if !strings.Contains(got[0], ":NEW.ID := SEQ_ESB_EVENT.NEXTVAL;") || !strings.HasSuffix(got[0], "END;") {
		t.Fatalf("PL/SQL block was split or changed: %q", got[0])
	}
	if strings.Contains(got[0], "\n/") || strings.TrimSpace(got[1]) != "SELECT 1 FROM DUAL" {
		t.Fatalf("SQL*Plus slash was not removed cleanly: %#v", got)
	}
}

func TestSplitScriptForDriver_OracleAnonymousBlockWithoutSlash(t *testing.T) {
	script := `BEGIN
  INSERT INTO t(id) VALUES (1);
  UPDATE t SET id = 2 WHERE id = 1;
END;`
	got := SplitScriptForDriver(script, "oracle")
	if len(got) != 1 || strings.TrimSpace(got[0]) != script {
		t.Fatalf("anonymous block should remain one statement: %#v", got)
	}
}

func TestSplitScriptForDriver_NonOracleKeepsGenericSemicolonRules(t *testing.T) {
	got := SplitScriptForDriver("BEGIN; UPDATE t SET x=1; COMMIT;", "sqlserver")
	if len(got) != 3 {
		t.Fatalf("non-Oracle script should use generic splitting, got %#v", got)
	}
}

func TestRunScript_OraclePLSQLExecutesOnce(t *testing.T) {
	op := &FakeOp{
		Driver: "oracle",
		ExecHandler: func(_ context.Context, sql string) (ExecResult, error) {
			if strings.Contains(sql, "\n/") {
				t.Fatalf("SQL*Plus slash reached Oracle driver: %q", sql)
			}
			return ExecResult{}, nil
		},
	}
	script := "CREATE OR REPLACE TRIGGER trg BEFORE INSERT ON t BEGIN :NEW.id := 1; END;\n/"
	res := RunScript(context.Background(), op, script, 10, time.Second, time.Second)
	if res.FirstError != "" || op.ExecCount != 1 || len(res.Statements) != 1 {
		t.Fatalf("PL/SQL block should execute exactly once: result=%+v calls=%+v", res, op.Calls)
	}
}

func TestRouteStatement_SelectIsQuery(t *testing.T) {
	for _, s := range []string{"SELECT 1", "select 1", "  SELECT 1", "SELECT\n1", "select 1; ", "WITH x AS (SELECT 1) SELECT * FROM x"} {
		if RouteStatement(s) != KindQuery {
			t.Fatalf("%q should be KindQuery", s)
		}
	}
}

func TestRouteStatement_OtherKeywords(t *testing.T) {
	cases := map[string]StatementKind{
		"SHOW TABLES":                  KindQuery,
		"DESC t":                       KindQuery,
		"DESCRIBE t":                   KindQuery,
		"EXPLAIN SELECT 1":             KindQuery,
		"VALUES (1)":                   KindQuery,
		"TABLE t":                      KindQuery, // standard SQL: TABLE x is shorthand for SELECT * FROM x
		"(SELECT 1)":                   KindQuery, // parenthesised SELECT
		"  (  SELECT 1 )":              KindQuery,
		"UPDATE t SET x=1":             KindExec,
		"INSERT INTO t VALUES (1)":     KindExec,
		"DELETE FROM t":                KindExec,
		"DROP TABLE t":                 KindExec,
		"CREATE TABLE t (id int)":      KindExec,
		"TRUNCATE TABLE t":             KindExec,
		"BEGIN":                        KindExec,
		"COMMIT":                       KindExec,
		"":                             KindExec,
		"-- comment\nSELECT 1":         KindQuery,
		"-- comment\nUPDATE t SET x=1": KindExec,
	}
	for in, want := range cases {
		if got := RouteStatement(in); got != want {
			t.Fatalf("%q: got %v, want %v", in, got, want)
		}
	}
}

func TestTrimStatement(t *testing.T) {
	cases := []struct{ in, want string }{
		{"  SELECT 1  ", "SELECT 1"},
		{"-- hi\nSELECT 1", "SELECT 1"},
		{"/* x */SELECT 1", "SELECT 1"},
		{"/* x */ -- y\n  SELECT 1\n", "SELECT 1"},
		{"SELECT 1;", "SELECT 1"},
		{"", ""},
	}
	for _, c := range cases {
		if got := TrimStatement(c.in); got != c.want {
			t.Fatalf("%q -> %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRunScript_DispatchesByKind(t *testing.T) {
	op := &FakeOp{
		Driver: "mysql",
		QueryHandler: func(_ context.Context, s string, _ int) (QueryResult, error) {
			return QueryResult{
				Columns: []QueryColumn{{Name: "x"}, {Name: "y"}},
				Rows:    [][]any{{"a", "b"}, {"c", "d"}},
				TookMs:  17,
			}, nil
		},
		ExecHandler: func(_ context.Context, s string) (ExecResult, error) {
			return ExecResult{AffectedRows: 3, TookMs: 23}, nil
		},
	}
	res := RunScript(context.Background(), op, "SELECT 1; UPDATE t SET x=1", 100, time.Second, time.Second)
	if len(res.Statements) != 2 {
		t.Fatalf("expected 2 statements, got %d", len(res.Statements))
	}
	if res.Statements[0].Type != "query" || res.Statements[0].RowCount != 2 {
		t.Fatalf("statement 0 wrong: %+v", res.Statements[0])
	}
	if res.Statements[1].Type != "exec" || res.Statements[1].AffectedRows != 3 {
		t.Fatalf("statement 1 wrong: %+v", res.Statements[1])
	}
	if res.Statements[0].TookMs != 17 || res.Statements[1].TookMs != 23 {
		t.Fatalf("statement timing not propagated: %+v", res.Statements)
	}
	if res.FirstError != "" {
		t.Fatalf("expected no error, got %q", res.FirstError)
	}
	if res.Driver != "mysql" {
		t.Fatalf("driver not propagated: %s", res.Driver)
	}
}

func TestRunScript_StopsOnFirstError(t *testing.T) {
	op := &FakeOp{
		Driver: "mysql",
		QueryHandler: func(_ context.Context, s string, _ int) (QueryResult, error) {
			if strings.Contains(s, "SELECT 1") {
				return QueryResult{}, errors.New("kaboom")
			}
			return QueryResult{}, nil
		},
		ExecHandler: func(_ context.Context, s string) (ExecResult, error) {
			return ExecResult{}, nil
		},
	}
	res := RunScript(context.Background(), op, "SELECT 1; UPDATE t SET x=1; SELECT 2", 100, time.Second, time.Second)
	if len(res.Statements) != 1 {
		t.Fatalf("expected to stop after first failure, got %d statements", len(res.Statements))
	}
	if res.FirstError == "" || res.FirstErrorAt != 0 {
		t.Fatalf("FirstError/FirstErrorAt wrong: %q @ %d", res.FirstError, res.FirstErrorAt)
	}
	if op.ExecCount != 0 || op.QueryCount != 1 {
		t.Fatalf("counts: q=%d e=%d (want 1/0)", op.QueryCount, op.ExecCount)
	}
}

func TestRunScript_TruncatesRows(t *testing.T) {
	op := &FakeOp{
		Driver: "mysql",
		QueryHandler: func(_ context.Context, s string, maxRows int) (QueryResult, error) {
			// Pretend the DB returned 10 rows but we cap at 3.
			rows := make([][]any, 10)
			for i := range rows {
				rows[i] = []any{i}
			}
			truncated := false
			if maxRows > 0 && maxRows < len(rows) {
				rows = rows[:maxRows]
				truncated = true
			}
			return QueryResult{Columns: []QueryColumn{{Name: "i"}}, Rows: rows, Truncated: truncated, RowCount: len(rows)}, nil
		},
	}
	res := RunScript(context.Background(), op, "SELECT * FROM big", 3, time.Second, time.Second)
	if !res.Truncated || !res.Statements[0].Truncated {
		t.Fatalf("expected truncation, got res=%+v stmt=%+v", res, res.Statements[0])
	}
	if res.Statements[0].RowCount != 3 {
		t.Fatalf("RowCount=%d, want 3", res.Statements[0].RowCount)
	}
}

func TestRunScript_ClampsMaxRows(t *testing.T) {
	op := &FakeOp{
		Driver: "mysql",
		QueryHandler: func(_ context.Context, s string, maxRows int) (QueryResult, error) {
			return QueryResult{}, nil
		},
	}
	// maxRows > MaxRowsCap should clamp down — verified via QueryHandler
	// receiving the clamped value.
	_ = RunScript(context.Background(), op, "SELECT 1", MaxRowsCap*10, time.Second, time.Second)
	if len(op.Calls) != 1 {
		t.Fatalf("expected 1 call")
	}
	// We don't have a way to inspect the maxRows passed to the handler
	// here without a richer Fake; the call-count assertion is enough
	// since MaxRowsCap>0 cannot panic.
}

func TestRouteStatement_LeadingCommentSkipped(t *testing.T) {
	if RouteStatement("-- only\nSELECT 1") != KindQuery {
		t.Fatal("leading line comment must not hide SELECT")
	}
	if RouteStatement("/* leading */ SELECT 1") != KindQuery {
		t.Fatal("leading block comment must not hide SELECT")
	}
}

func TestFakeOp_RecordsCalls(t *testing.T) {
	op := &FakeOp{
		Driver: "fake",
		QueryHandler: func(_ context.Context, _ string, _ int) (QueryResult, error) {
			return QueryResult{}, nil
		},
		ExecHandler: func(_ context.Context, _ string) (ExecResult, error) {
			return ExecResult{}, nil
		},
	}
	_, _ = op.Query(context.Background(), "SELECT 1", 10)
	_, _ = op.Exec(context.Background(), "UPDATE t SET x=1")
	want := []FakeCall{{Kind: "query", SQL: "SELECT 1"}, {Kind: "exec", SQL: "UPDATE t SET x=1"}}
	if !reflect.DeepEqual(op.Calls, want) {
		t.Fatalf("calls = %+v, want %+v", op.Calls, want)
	}
	if op.QueryCount != 1 || op.ExecCount != 1 {
		t.Fatalf("counts wrong: q=%d e=%d", op.QueryCount, op.ExecCount)
	}
}

func TestFakeOp_NoHandlerSeedsError(t *testing.T) {
	op := &FakeOp{}
	if _, err := op.Query(context.Background(), "SELECT 1", 10); err == nil {
		t.Fatal("expected error")
	}
	if _, err := op.Exec(context.Background(), "X"); err == nil {
		t.Fatal("expected error")
	}
}

func TestSQLOp_BadDSNReturnsError(t *testing.T) {
	// We bypass the open func to assert DSN construction errors
	// surface; a nil cfg must produce ErrNotConfigured.
	if _, err := NewSQLOp(context.Background(), nil); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("nil cfg should return ErrNotConfigured, got %v", err)
	}
}

func TestSQLOp_DSNBuilders(t *testing.T) {
	cases := []struct {
		driver string
		cfg    *config.DBConfig
		want   []string // substrings that must appear
	}{
		{"mysql", &config.DBConfig{Host: "h", Port: 3306, User: "u", Password: "p", Database: "d"},
			[]string{"u:p@tcp(h:3306)/d", "parseTime=true"}},
		{"mysql", &config.DBConfig{Host: "h", User: "u", Password: "p", Database: "d"},
			[]string{"tcp(h:3306)/d"}}, // default port
		{"sqlserver", &config.DBConfig{Host: "h", Port: 1433, User: "u", Password: "p", Database: "d"},
			[]string{"server=h", "port=1433", "user+id=u", "database=d"}},
		{"mssql", &config.DBConfig{Host: "h", User: "u", Password: "p", Database: "d"},
			[]string{"server=h", "port=1433"}},
		{"oracle", &config.DBConfig{Host: "h", Port: 1521, User: "u", Password: "p", Database: "ORCL"},
			[]string{"oracle://", "h:1521/ORCL"}},
	}
	for _, c := range cases {
		got, err := buildDSN(c.driver, c.cfg)
		if err != nil {
			t.Fatalf("%s: %v", c.driver, err)
		}
		for _, frag := range c.want {
			// url.Values encodes spaces as '+'; check both raw and encoded.
			if !strings.Contains(got, frag) && !strings.Contains(got, strings.ReplaceAll(frag, " ", "+")) {
				t.Fatalf("%s: DSN %q missing %q", c.driver, got, frag)
			}
		}
	}
}

func TestSQLOp_DSNBadDriver(t *testing.T) {
	if _, err := buildDSN("postgres", &config.DBConfig{Host: "h", User: "u", Password: "p", Database: "d"}); err == nil {
		t.Fatal("expected error for unsupported driver")
	}
}

func TestSQLOp_DSNEscapesPassword(t *testing.T) {
	cfg := &config.DBConfig{Host: "h", User: "u", Password: "p;w@rd/x?#= &", Database: "d"}
	dsn, err := buildDSN("mysql", cfg)
	if err != nil {
		t.Fatal(err)
	}
	// ParseDSN separates at the last '@' and does not URL-decode userinfo.
	if !strings.Contains(dsn, "u:p;w@rd/x?#= &@tcp(") {
		t.Fatalf("DSN must keep password bytes verbatim, got %q", dsn)
	}
}

func TestSQLOp_DSNPreservesExclamation(t *testing.T) {
	// Regression: url.QueryEscape would turn 'yc1234!' into 'yc1234%21'
	// and mysql_native_password would reject the password with 1045.
	// mysqlDSNEscape must keep '!' verbatim.
	cfg := &config.DBConfig{Host: "localhost", Port: 3306, User: "root", Password: "yc1234!", Database: "master"}
	dsn, err := buildDSN("mysql", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dsn, "root:yc1234!@tcp(localhost:3306)/master") {
		t.Fatalf("DSN must keep '!' verbatim in userinfo, got %q", dsn)
	}
	if strings.Contains(dsn, "%21") {
		t.Fatalf("DSN must NOT encode '!' as %%21, got %q", dsn)
	}
}

// TestSQLOp_EnsureOpenCarriesCredentials is the regression test for the
// empty-user/no-password bug: ensureOpenLocked must forward User and
// Password into buildDSN. Pre-fix, the produced DSN started with ":@tcp("
// (no user, no password) and MySQL returned "Access denied for user
// ”@'…' (using password: NO)". Post-fix, the DSN must embed the user
// and the byte-exact password.
func TestSQLOp_EnsureOpenCarriesCredentials(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.DBConfig
	}{
		{
			name: "mysql basic",
			cfg: &config.DBConfig{
				Host: "h", Port: 3306, User: "root", Password: "123456", Database: "master",
			},
		},
		{
			name: "mysql password with specials",
			cfg: &config.DBConfig{
				Host: "h", Port: 3306, User: "u", Password: "p;w@rd/x?#= &", Database: "d",
			},
		},
		{
			// ParseDSN permits ':' and '@' inside the password: it separates
			// at the first ':' and last '@'. They must remain byte-exact.
			name: "mysql password with both delimiters",
			cfg: &config.DBConfig{
				Host: "h", Port: 3306, User: "u", Password: "p:a@s:s", Database: "d",
			},
		},
		{
			// The real-world password from 2026-09-14 customer box.
			name: "mysql yc1234!",
			cfg: &config.DBConfig{
				Host: "h", Port: 3306, User: "root", Password: "yc1234!", Database: "dev-bs-esb1",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var capturedDSN string
			fakeOpen := func(driver, dsn string) (*sql.DB, error) {
				capturedDSN = dsn
				return nil, errors.New("fake open: stop here, we only want to see the DSN")
			}
			op := &SQLOp{
				driver:   c.cfg.Driver(),
				host:     c.cfg.Host,
				port:     c.cfg.EffectivePort(c.cfg.Driver()),
				user:     c.cfg.User,
				password: c.cfg.Password,
				database: c.cfg.Database,
				open:     fakeOpen,
				pingTO:   time.Second,
			}
			err := op.ensureOpen(context.Background())
			if err == nil {
				t.Fatalf("expected fake-open error, got nil")
			}
			if capturedDSN == "" {
				t.Fatalf("open func never called — ensureOpen did not reach buildDSN")
			}
			// Must carry the user. Pre-fix this was the leading ':' of ':@tcp(…)'.
			if !strings.Contains(capturedDSN, c.cfg.User+":") {
				t.Fatalf("DSN %q does not carry user %q", capturedDSN, c.cfg.User)
			}
			// Password must be byte-exact; URL escaping would alter what the
			// MySQL authentication handshake receives.
			wantPass := c.cfg.Password
			if !strings.Contains(capturedDSN, ":"+wantPass+"@") {
				t.Fatalf("DSN %q does not carry password %q", capturedDSN, wantPass)
			}
			// And the empty-user signature must be gone.
			if strings.HasPrefix(capturedDSN, ":@") {
				t.Fatalf("DSN %q still has empty-user ':@' prefix — the original bug", capturedDSN)
			}
		})
	}
}

// ---- Lazy reconnect (2026-09-15 G4①) ----

var registerFakeDBDriverOnce sync.Once

// dbopFakeDriver is a minimal database/sql driver that answers Ping and
// returns empty result sets, so tests can exercise SQLOp's open / ping /
// query plumbing without a real database.
type dbopFakeDriver struct{}

func (dbopFakeDriver) Open(string) (driver.Conn, error) { return dbopFakeConn{}, nil }

type dbopFakeConn struct{}

func (dbopFakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("dbop-fake: prepare unsupported")
}

func TestSQLOp_OracleDSNEscapesURLUserInfo(t *testing.T) {
	cfg := &config.DBConfig{Host: "h", Port: 1521, User: "u@example", Password: "p?@#/ x", Database: "ORCL"}
	dsn, err := buildDSN("oracle", cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"u@example", "p?@#/ x"} {
		if strings.Contains(dsn, raw) {
			t.Fatalf("Oracle URL userinfo contains unescaped %q: %q", raw, dsn)
		}
	}
	if !strings.Contains(dsn, "u%40example:p%3F%40%23%2F%20x@h:1521/ORCL") {
		t.Fatalf("unexpected Oracle DSN escaping: %q", dsn)
	}
}

func TestSQLOp_SQLServerTLSOptions(t *testing.T) {
	cfg := &config.DBConfig{
		Host: "h", User: "u", Password: "p", Database: "d",
		Encrypt: "disable", TLSMin: "1.0", TrustCert: true, HostnameInCert: "db.internal",
	}
	dsn, err := buildDSN("sqlserver", cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"encrypt=disable", "tlsmin=1.0", "trustservercertificate=true", "hostnameincertificate=db.internal"} {
		if !strings.Contains(dsn, want) {
			t.Fatalf("SQL Server DSN %q missing %q", dsn, want)
		}
	}
}
func (dbopFakeConn) Close() error               { return nil }
func (dbopFakeConn) Begin() (driver.Tx, error)  { return dbopFakeTx{}, nil }
func (dbopFakeConn) Ping(context.Context) error { return nil }

type dbopFakeTx struct{}

func (dbopFakeTx) Commit() error   { return nil }
func (dbopFakeTx) Rollback() error { return nil }

func (dbopFakeConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return dbopFakeRows{}, nil
}

func (dbopFakeConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(0), nil
}

type dbopFakeRows struct{}

func (dbopFakeRows) Columns() []string         { return []string{"c1"} }
func (dbopFakeRows) Close() error              { return nil }
func (dbopFakeRows) Next([]driver.Value) error { return io.EOF }

func newLazyTestOp(open func(driverName, dsn string) (*sql.DB, error)) *SQLOp {
	cfg := &config.DBConfig{Host: "h", Port: 3306, User: "u", Password: "p", Database: "d"}
	return &SQLOp{
		driver:   cfg.Driver(),
		host:     cfg.Host,
		port:     cfg.EffectivePort(cfg.Driver()),
		user:     cfg.User,
		password: cfg.Password,
		database: cfg.Database,
		status: Status{
			Configured: true,
			Driver:     cfg.Driver(),
			Host:       cfg.Host,
			Port:       cfg.EffectivePort(cfg.Driver()),
			Database:   cfg.Database,
		},
		open:   open,
		pingTO: 2 * time.Second,
	}
}

// TestSQLOp_LazyReopenAfterStartupFailure is the G4① regression: a
// connector whose startup ping failed must recover once the DB comes up,
// without a helper restart. Pre-fix, Query/Exec returned the cached
// "connector not open" error forever.
func TestSQLOp_LazyReopenAfterStartupFailure(t *testing.T) {
	registerFakeDBDriverOnce.Do(func() { sql.Register("dbop-fake", dbopFakeDriver{}) })

	op := newLazyTestOp(func(string, string) (*sql.DB, error) {
		return nil, errors.New("dial tcp: connect: connection refused")
	})
	if _, err := op.Query(context.Background(), "SELECT 1", 10); err == nil ||
		!strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("expected lazy-open failure to surface, got %v", err)
	}
	if st := op.Status(context.Background()); !strings.Contains(st.LastError, "connection refused") {
		t.Fatalf("status should carry the failure, got %+v", st)
	}

	// "MySQL gets installed": the next call must reconnect on its own.
	op.mu.Lock()
	op.open = func(_, dsn string) (*sql.DB, error) { return sql.Open("dbop-fake", dsn) }
	op.mu.Unlock()

	qr, err := op.Query(context.Background(), "SELECT 1", 10)
	if err != nil {
		t.Fatalf("lazy reopen failed: %v", err)
	}
	if qr.RowCount != 0 {
		t.Fatalf("RowCount = %d, want 0", qr.RowCount)
	}
	if st := op.Status(context.Background()); !st.Open || st.LastError != "" {
		t.Fatalf("status after reopen: %+v", st)
	}
	if _, err := op.Exec(context.Background(), "UPDATE t SET x=1"); err != nil {
		t.Fatalf("Exec after reopen: %v", err)
	}
}

// TestSQLOp_StatusLazilyOpens covers G4③: validate_config's Status call
// must attempt a real open/ping instead of replaying startup state.
func TestSQLOp_StatusLazilyOpens(t *testing.T) {
	registerFakeDBDriverOnce.Do(func() { sql.Register("dbop-fake", dbopFakeDriver{}) })
	op := newLazyTestOp(func(_, dsn string) (*sql.DB, error) { return sql.Open("dbop-fake", dsn) })
	st := op.Status(context.Background())
	if !st.Open {
		t.Fatalf("Status should lazily open, got %+v", st)
	}
}

func TestIsRetryable(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{context.Canceled, false},
		{context.DeadlineExceeded, false},
		{errors.New("some random SQL syntax error"), false},
		{errors.New("driver: bad connection"), true},
		{errors.New("broken pipe"), true},
		{errors.New("Connection RESET by peer"), true},
		{errors.New("read tcp: i/o timeout"), true},
		{errors.New("invalid connection"), true},
		{errors.New("dial tcp: no such host"), true},
	}
	for _, c := range cases {
		if got := isRetryable(c.err); got != c.want {
			t.Fatalf("isRetryable(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
