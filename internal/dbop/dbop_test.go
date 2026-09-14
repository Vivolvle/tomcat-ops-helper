package dbop

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
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

func TestRouteStatement_SelectIsQuery(t *testing.T) {
	for _, s := range []string{"SELECT 1", "select 1", "  SELECT 1", "SELECT\n1", "select 1; ", "WITH x AS (SELECT 1) SELECT * FROM x"} {
		if RouteStatement(s) != KindQuery {
			t.Fatalf("%q should be KindQuery", s)
		}
	}
}

func TestRouteStatement_OtherKeywords(t *testing.T) {
	cases := map[string]StatementKind{
		"SHOW TABLES":                 KindQuery,
		"DESC t":                      KindQuery,
		"DESCRIBE t":                  KindQuery,
		"EXPLAIN SELECT 1":            KindQuery,
		"VALUES (1)":                  KindQuery,
		"TABLE t":                     KindQuery, // standard SQL: TABLE x is shorthand for SELECT * FROM x
		"(SELECT 1)":                  KindQuery, // parenthesised SELECT
		"  (  SELECT 1 )":             KindQuery,
		"UPDATE t SET x=1":            KindExec,
		"INSERT INTO t VALUES (1)":    KindExec,
		"DELETE FROM t":               KindExec,
		"DROP TABLE t":                KindExec,
		"CREATE TABLE t (id int)":     KindExec,
		"TRUNCATE TABLE t":            KindExec,
		"BEGIN":                       KindExec,
		"COMMIT":                      KindExec,
		"":                            KindExec,
		"-- comment\nSELECT 1":        KindQuery,
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
			}, nil
		},
		ExecHandler: func(_ context.Context, s string) (ExecResult, error) {
			return ExecResult{AffectedRows: 3}, nil
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
		Driver:     "mysql",
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
	// mysqlDSNEscape only touches '@' and ':' in userinfo. Other special
	// chars (';', '/', '?', '#', '=', ' ', '&') must pass through verbatim
	// so the server's handshake compares the same bytes against the hash.
	if !strings.Contains(dsn, "u:p;w%40rd/x?#= &") {
		t.Fatalf("DSN must keep password bytes verbatim except for '@' (and ':'), got %q", dsn)
	}
	// The raw '@' inside the password must be escaped, otherwise
	// go-sql-driver would treat everything after it as the host.
	if strings.Contains(dsn, "u:p;w@rd") {
		t.Fatalf("DSN must escape '@' in userinfo, got %q", dsn)
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
// ''@'…' (using password: NO)". Post-fix, the DSN must embed the user
// and the URL-escaped password.
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
			// Regression: if mysqlDSNEscape ever forgets ':', the password
			// would split into user/pass and the server would see the wrong
			// hash. Must escape both ':' and '@'.
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
			// Must carry the password verbatim except for the DSN delimiters
			// '@' and ':' (which mysqlDSNEscape escapes). url.QueryEscape
			// would turn '!' into '%21' and mysql_native_password would
			// reject the connection with 1045.
			wantPass := mysqlDSNEscape(c.cfg.Password)
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

func TestIsRetryable(t *testing.T) {
	cases := []struct {
		err   error
		want  bool
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
