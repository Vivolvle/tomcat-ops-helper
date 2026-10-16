package dbop

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"
)

type formSQLDriver struct{}
type formSQLConn struct{ dbopFakeConn }
type formSQLRows struct{ done bool }

func (formSQLDriver) Open(string) (driver.Conn, error) { return formSQLConn{}, nil }
func (formSQLConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if !strings.Contains(q, "CASE WHEN DBMS_LOB.GETLENGTH(FORMENTITY) <= 8388608") || !strings.HasSuffix(q, "FORMID = :1") || len(args) != 1 || args[0].Value != "619fhq907y4pqbl1" {
		return nil, errors.New("query is not bounded and bound")
	}
	return &formSQLRows{}, nil
}
func (*formSQLRows) Columns() []string { return []string{"bytes", "blob"} }
func (*formSQLRows) Close() error      { return nil }
func (r *formSQLRows) Next(v []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	v[0] = int64(3)
	v[1] = []byte("MJU")
	return nil
}
func TestFormBoundReadWithReadOnlyLock(t *testing.T) {
	sql.Register("form-sql-test", formSQLDriver{})
	pool, err := sql.Open("form-sql-test", "")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	o := &SQLOp{driver: "oracle", forbidCmd: true, db: pool}
	data, err := o.ReadFormEntity(context.Background(), "619fhq907y4pqbl1")
	if err != nil || string(data) != "MJU" {
		t.Fatalf("read-only bound read: %q %v", data, err)
	}
}
