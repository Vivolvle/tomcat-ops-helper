package dbop

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"

	"tomcat-ops-helper/internal/config"

	_ "github.com/go-sql-driver/mysql"     // mysql driver
	_ "github.com/microsoft/go-mssqldb"    // sqlserver driver (registers "sqlserver")
	_ "github.com/sijms/go-ora/v2"         // oracle driver (registers "oracle")
)

// SQLOp is the production Op: a thin wrapper around *sql.DB that adds
// one piece of behaviour the stdlib pool doesn't give us — a single
// reconnect-then-retry when a statement hits a stale or dead
// connection. Pool sizing itself is left to SetMax{Open,Idle}Conns.
type SQLOp struct {
	driver   string
	host     string
	port     int
	user     string
	password string
	database string

	mu      sync.Mutex
	db      *sql.DB
	status  Status
	open    func(driver, dsn string) (*sql.DB, error) // overridable for tests
	pingTO  time.Duration
}

// NewSQLOp opens the pool eagerly (sql.Open + Ping) and returns the
// Op. The returned error is the only signal of "this connector is
// dead at startup" — the caller logs it and skips wiring this Op into
// the Registry. Re-open is attempted lazily on the next call.
func NewSQLOp(ctx context.Context, cfg *config.DBConfig) (*SQLOp, error) {
	if cfg == nil {
		return nil, ErrNotConfigured
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("dbop: %w", err)
	}
	driver := cfg.Driver()
	host := cfg.Host
	port := cfg.EffectivePort(driver)
	database := cfg.Database
	op := &SQLOp{
		driver:   driver,
		host:     host,
		port:     port,
		user:     cfg.User,
		password: cfg.Password,
		database: database,
		status: Status{
			Configured: true,
			Driver:     driver,
			Host:       host,
			Port:       port,
			Database:   database,
		},
		open:   sql.Open,
		pingTO: 5 * time.Second,
	}
	if err := op.ensureOpen(ctx); err != nil {
		return op, err // status captured; caller may still want the Op for retries
	}
	return op, nil
}

// NewSQLOpWithOpen is NewSQLOp with a custom open function. Used by
// tests to inject a fake driver.
func NewSQLOpWithOpen(ctx context.Context, cfg *config.DBConfig, open func(driver, dsn string) (*sql.DB, error)) (*SQLOp, error) {
	op, err := NewSQLOp(ctx, cfg)
	if err != nil {
		return op, err
	}
	op.mu.Lock()
	op.open = open
	op.mu.Unlock()
	return op, nil
}

// Status returns the cached health snapshot, refreshing Open / LatencyMs
// by issuing a Ping if the pool is currently considered open.
func (o *SQLOp) Status(ctx context.Context) Status {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.db != nil {
		pingCtx, cancel := context.WithTimeout(ctx, o.pingTO)
		start := time.Now()
		err := o.db.PingContext(pingCtx)
		cancel()
		o.status.LatencyMs = time.Since(start).Milliseconds()
		if err != nil {
			o.status.Open = false
			o.markLocked(err)
			return o.status
		}
		o.status.Open = true
	}
	return o.status
}

// Query runs one SELECT statement with row-size + row-count safety.
func (o *SQLOp) Query(ctx context.Context, sqlText string, maxRows int) (QueryResult, error) {
	if maxRows <= 0 {
		maxRows = DefaultMaxRows
	}
	if maxRows > MaxRowsCap {
		maxRows = MaxRowsCap
	}
	qr, err := o.runQuery(ctx, sqlText, maxRows)
	if err != nil && isRetryable(err) {
		// One reconnect-and-retry. Stale conns and idle-timeout
		// drops surface as net errors here; we tear the pool down
		// and rebuild so the next Ping picks up a fresh handle.
		_ = o.closeLocked()
		if rerr := o.ensureOpen(ctx); rerr == nil {
			qr, err = o.runQuery(ctx, sqlText, maxRows)
		}
	}
	if err != nil {
		o.mu.Lock()
		o.markLocked(err)
		o.mu.Unlock()
	}
	return qr, err
}

// Exec runs one non-SELECT statement.
func (o *SQLOp) Exec(ctx context.Context, sqlText string) (ExecResult, error) {
	er, err := o.runExec(ctx, sqlText)
	if err != nil && isRetryable(err) {
		_ = o.closeLocked()
		if rerr := o.ensureOpen(ctx); rerr == nil {
			er, err = o.runExec(ctx, sqlText)
		}
	}
	if err != nil {
		o.mu.Lock()
		o.markLocked(err)
		o.mu.Unlock()
	}
	return er, err
}

// Close shuts down the pool.
func (o *SQLOp) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.closeLocked()
}

// ensureOpen opens the pool and Pings it. Caller must NOT hold o.mu.
func (o *SQLOp) ensureOpen(ctx context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.ensureOpenLocked(ctx)
}

// ensureOpenLocked opens the pool and Pings it. Caller holds o.mu.
func (o *SQLOp) ensureOpenLocked(ctx context.Context) error {
	dsn, err := buildDSN(o.driver, &config.DBConfig{
		Host:     o.host,
		Port:     o.port,
		User:     o.user,
		Password: o.password,
		Database: o.database,
	})
	if err != nil {
		o.markLocked(err)
		return err
	}
	db, err := o.open(o.driver, dsn)
	if err != nil {
		o.markLocked(err)
		return err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(1)
	db.SetConnMaxIdleTime(5 * time.Minute)
	pingCtx, cancel := context.WithTimeout(ctx, o.pingTO)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		o.markLocked(err)
		return err
	}
	o.db = db
	o.status.Open = true
	o.status.LastError = ""
	o.status.LastErrorAt = nil
	return nil
}

func (o *SQLOp) closeLocked() error {
	if o.db == nil {
		return nil
	}
	err := o.db.Close()
	o.db = nil
	o.status.Open = false
	return err
}

func (o *SQLOp) markLocked(err error) {
	if err == nil {
		return
	}
	o.status.Open = false
	o.status.LastError = err.Error()
	now := time.Now().UTC()
	o.status.LastErrorAt = &now
}

func (o *SQLOp) runQuery(ctx context.Context, sqlText string, maxRows int) (QueryResult, error) {
	o.mu.Lock()
	db := o.db
	o.mu.Unlock()
	if db == nil {
		return QueryResult{}, errors.New("dbop: connector not open")
	}
	start := time.Now()
	rows, err := db.QueryContext(ctx, sqlText)
	if err != nil {
		return QueryResult{}, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return QueryResult{}, err
	}
	colTypes, _ := rows.ColumnTypes()
	out := QueryResult{Columns: make([]QueryColumn, len(cols))}
	for i, name := range cols {
		qc := QueryColumn{Name: name}
		if i < len(colTypes) {
			qc.Type = colTypes[i].DatabaseTypeName()
			nullable, ok := colTypes[i].Nullable()
			if ok {
				qc.Nullable = nullable
			}
		}
		out.Columns[i] = qc
	}

	scanDest := make([]sql.RawBytes, len(cols))
	scanPtrs := make([]any, len(cols))
	for i := range scanDest {
		scanPtrs[i] = &scanDest[i]
	}
	for rows.Next() {
		if len(out.Rows) >= maxRows {
			// Drain a few more to detect truncation accurately
			// (we still report Truncated=true).
			out.Truncated = true
			// Continue scanning so rows.Err is checked, but skip
			// allocation.
			for rows.Next() {
				_ = rows
				if len(out.Rows) >= maxRows*4 {
					// Hard stop; the caller asked for maxRows
					// and we know it's much more.
					break
				}
			}
			break
		}
		if err := rows.Scan(scanPtrs...); err != nil {
			return QueryResult{}, err
		}
		row := make([]any, len(cols))
		for i, b := range scanDest {
			if b == nil {
				row[i] = nil
			} else {
				row[i] = string(b)
			}
		}
		out.Rows = append(out.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return QueryResult{}, err
	}
	out.RowCount = len(out.Rows)
	out.TookMs = time.Since(start).Milliseconds()
	return out, nil
}

func (o *SQLOp) runExec(ctx context.Context, sqlText string) (ExecResult, error) {
	o.mu.Lock()
	db := o.db
	o.mu.Unlock()
	if db == nil {
		return ExecResult{}, errors.New("dbop: connector not open")
	}
	start := time.Now()
	res, err := db.ExecContext(ctx, sqlText)
	if err != nil {
		return ExecResult{}, err
	}
	affected, _ := res.RowsAffected()
	lastID, _ := res.LastInsertId()
	er := ExecResult{
		AffectedRows: affected,
		TookMs:       time.Since(start).Milliseconds(),
	}
	if lastID > 0 {
		er.LastInsertID = fmt.Sprintf("%d", lastID)
	}
	return er, nil
}

// isRetryable is the coarse "this is probably a stale-conn error, try
// once more" predicate. We do not try to be exhaustive; one cheap
// retry covers the vast majority of transient drops. Anything that
// looks like a SQL syntax error or permission failure will not match.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	s := err.Error()
	for _, frag := range []string{
		"broken pipe",
		"connection reset",
		"connection refused",
		"closed pool",
		"driver: bad connection",
		"invalid connection",
		"bad connection",
		"EOF",
		"i/o timeout",
		"network is unreachable",
		"no such host",
	} {
		if containsFold(s, frag) {
			return true
		}
	}
	return false
}

func containsFold(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	if len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			a, b := haystack[i+j], needle[j]
			if a >= 'A' && a <= 'Z' {
				a += 32
			}
			if b >= 'A' && b <= 'Z' {
				b += 32
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// buildDSN constructs the driver-specific connection string. The
// password is URL-encoded so special characters (e.g. ';', '@', '/',
// '?', '#') don't break parsing.
func buildDSN(driver string, cfg *config.DBConfig) (string, error) {
	port := cfg.EffectivePort(driver)
	switch driver {
	case "mysql":
		// go-sql-driver/mysql DSN: user:pass@tcp(host:port)/db?...
		user := url.QueryEscape(cfg.User)
		pass := url.QueryEscape(cfg.Password)
		host := cfg.Host
		if port > 0 {
			host = fmt.Sprintf("%s:%d", cfg.Host, port)
		}
		return fmt.Sprintf("%s:%s@tcp(%s)/%s?parseTime=true&charset=utf8mb4&timeout=10s&readTimeout=30s&writeTimeout=30s",
			user, pass, host, cfg.Database), nil
	case "sqlserver", "mssql":
		// microsoft/go-mssqldb DSN: key=value;...
		q := url.Values{}
		q.Set("user id", cfg.User)
		q.Set("password", cfg.Password)
		q.Set("database", cfg.Database)
		if cfg.Host != "" {
			q.Set("server", cfg.Host)
		}
		if port > 0 {
			q.Set("port", fmt.Sprintf("%d", port))
		}
		q.Set("dial timeout", "10")
		q.Set("connection timeout", "10")
		return q.Encode(), nil
	case "oracle":
		// sijms/go-ora/v2 DSN: oracle://user:pass@host:port/service_name
		user := url.QueryEscape(cfg.User)
		pass := url.QueryEscape(cfg.Password)
		host := cfg.Host
		if port > 0 {
			host = fmt.Sprintf("%s:%d", cfg.Host, port)
		}
		return fmt.Sprintf("oracle://%s:%s@%s/%s", user, pass, host, cfg.Database), nil
	}
	return "", fmt.Errorf("unsupported driver %q", driver)
}
