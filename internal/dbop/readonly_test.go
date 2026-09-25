package dbop

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

func TestCheckReadOnlyQuery(t *testing.T) {
	cases := []struct {
		name string
		stmt string
		want bool // true = allowed
	}{
		{"plain select", "SELECT * FROM esb_route", true},
		{"lowercase select", "select 1", true},
		{"leading line comment", "-- ops note\nSELECT 1", true},
		{"leading block comment", "/* ops */ SELECT 1", true},
		{"compound paren select", "(SELECT 1) UNION (SELECT 2)", true},
		{"show create table", "SHOW CREATE TABLE esb_route", true},
		{"show processlist", "SHOW PROCESSLIST", true},
		{"desc", "DESC esb_route", true},
		{"describe", "describe esb_route", true},
		{"explain select", "EXPLAIN SELECT * FROM esb_route", true},
		{"explain analyze select", "EXPLAIN ANALYZE SELECT * FROM esb_route", true},
		{"dml word inside string literal", "SELECT * FROM t WHERE name = 'DELETE FROM x; DROP TABLE y'", true},
		{"dml word inside string literal 2", "SELECT * FROM esb_route WHERE error_message = 'update failed'", true},
		{"dml prefix inside identifier", "SELECT * FROM update_log", true},
		{"outfile inside string literal", "SELECT 'INTO OUTFILE' AS tag FROM t", true},
		{"quoted identifier", "SELECT \"insert\" FROM t", true},
		{"backtick identifier", "SELECT `delete` FROM t", true},

		{"insert", "INSERT INTO t VALUES (1)", false},
		{"update", "UPDATE t SET x = 1", false},
		{"delete", "DELETE FROM t", false},
		{"truncate", "TRUNCATE TABLE t", false},
		{"drop", "DROP TABLE t", false},
		{"create", "CREATE TABLE t (x INT)", false},
		{"alter", "ALTER TABLE t ADD y INT", false},
		{"replace", "REPLACE INTO t VALUES (1)", false},
		{"merge", "MERGE INTO t USING s ON (t.id = s.id) WHEN MATCHED THEN UPDATE SET t.x = s.x", false},
		{"call", "CALL p()", false},
		{"grant", "GRANT ALL ON *.* TO u", false},
		{"set", "SET @x = 1", false},
		{"lock tables", "LOCK TABLES t WRITE", false},
		{"begin", "BEGIN", false},
		{"commit", "COMMIT", false},
		{"with delete smuggling", "WITH d AS (SELECT * FROM t) DELETE FROM t USING d", false},
		{"with update smuggling", "WITH c AS (SELECT 1) UPDATE t SET x = c", false},
		{"plain with select also blocked", "WITH c AS (SELECT 1) SELECT * FROM c", false},
		{"explain delete", "EXPLAIN DELETE FROM t", false},
		{"explain analyze delete", "EXPLAIN ANALYZE DELETE FROM t", false},
		{"explain insert", "EXPLAIN INSERT INTO t VALUES (1)", false},
		{"explain bare", "EXPLAIN", false},
		{"into outfile", "SELECT * FROM t INTO OUTFILE '/tmp/x'", false},
		{"into dumpfile", "SELECT * FROM t INTO DUMPFILE '/tmp/x'", false},
		{"empty", "", false},
		{"only comment", "-- nothing to see\n", false},
		{"only whitespace", "   ", false},
		{"paren wrapped dml", "(DELETE FROM t)", false},
	}
	for _, tc := range cases {
		err := checkReadOnlyQuery(tc.stmt)
		if tc.want && err != nil {
			t.Errorf("%s: checkReadOnlyQuery(%q) = %v, want allowed", tc.name, tc.stmt, err)
		}
		if !tc.want && !errors.Is(err, ErrForbidCmd) {
			t.Errorf("%s: checkReadOnlyQuery(%q) = %v, want ErrForbidCmd", tc.name, tc.stmt, err)
		}
	}
}

func TestScanSQLTokens(t *testing.T) {
	// Quoted identifiers and string literals produce no tokens; word
	// chars are uppercased; comments vanish.
	got := scanSQLTokens("-- c1\n/* c2 */ SELECT a.col, 'two words' FROM `tbl` x")
	want := []string{"SELECT", "A", "COL", "FROM", "X"}
	if len(got) != len(want) {
		t.Fatalf("tokens = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("token[%d] = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
}

// TestSQLOp_ForbidCmd exercises the enforcement inside SQLOp with the
// fake driver: Exec never reaches the driver, non-read-only Query
// shapes are refused, and plain SELECTs still work.
func TestSQLOp_ForbidCmd(t *testing.T) {
	registerFakeDBDriverOnce.Do(func() { sql.Register("dbop-fake", dbopFakeDriver{}) })
	op := newLazyTestOp(func(_, dsn string) (*sql.DB, error) { return sql.Open("dbop-fake", dsn) })
	op.forbidCmd = true

	if !op.ReadOnly() {
		t.Fatal("ReadOnly() should report the lock")
	}

	qr, err := op.Query(context.Background(), "SELECT 1", 10)
	if err != nil {
		t.Fatalf("plain SELECT must still work on a locked connector: %v", err)
	}
	if qr.RowCount != 0 {
		t.Fatalf("RowCount = %d, want 0", qr.RowCount)
	}

	for _, stmt := range []string{
		"WITH d AS (SELECT 1) DELETE FROM t USING d", // smuggled past the router as a query
		"EXPLAIN DELETE FROM t",
		"SELECT * FROM t INTO OUTFILE '/tmp/x'",
	} {
		_, err := op.Query(context.Background(), stmt, 10)
		if !errors.Is(err, ErrForbidCmd) {
			t.Errorf("Query(%q) error = %v, want ErrForbidCmd", stmt, err)
		}
	}

	_, err = op.Exec(context.Background(), "UPDATE t SET x = 1")
	if !errors.Is(err, ErrForbidCmd) {
		t.Errorf("Exec error = %v, want ErrForbidCmd", err)
	}

	st := op.Status(context.Background())
	// Policy rejections must NOT poison connector health.
	if strings.Contains(st.LastError, "forbid_cmd") {
		t.Fatalf("policy rejection leaked into connector health: LastError=%q", st.LastError)
	}
	if !st.Open {
		t.Fatalf("connector must stay open after policy rejections, status %+v", st)
	}
	if !st.ReadOnly {
		t.Fatal("Status.ReadOnly should be true on a locked connector")
	}
}

// TestSQLOp_ForbidCmdUnlockedByDefault sanity-checks the default: no
// forbid_cmd in config → writes still flow.
func TestSQLOp_ForbidCmdUnlockedByDefault(t *testing.T) {
	registerFakeDBDriverOnce.Do(func() { sql.Register("dbop-fake", dbopFakeDriver{}) })
	op := newLazyTestOp(func(_, dsn string) (*sql.DB, error) { return sql.Open("dbop-fake", dsn) })
	if _, err := op.Exec(context.Background(), "UPDATE t SET x = 1"); err != nil {
		t.Fatalf("Exec without forbid_cmd should succeed, got %v", err)
	}
	if op.ReadOnly() {
		t.Fatal("ReadOnly() should be false by default")
	}
}
