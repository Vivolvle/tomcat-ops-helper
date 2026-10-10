package dbop

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"tomcat-ops-helper/internal/config"

	_ "github.com/go-sql-driver/mysql"  // mysql driver
	_ "github.com/microsoft/go-mssqldb" // sqlserver driver (registers "sqlserver")
	_ "github.com/sijms/go-ora/v2"      // oracle driver (registers "oracle")
)

// SQLOp is the production Op: a thin wrapper around *sql.DB that adds
// one piece of behaviour the stdlib pool doesn't give us — a single
// reconnect-then-retry when a statement hits a stale or dead
// connection. Pool sizing itself is left to SetMax{Open,Idle}Conns.
type SQLOp struct {
	driver         string
	host           string
	port           int
	user           string
	password       string
	database       string
	encrypt        string
	tlsMin         string
	trustCert      bool
	hostnameInCert string
	// forbidCmd locks the connector to read-only (config.json
	// "forbid_cmd": true). Enforced in Exec / Query before any driver
	// call, so a locked connector can never reach the database with a
	// data-changing statement regardless of which code path calls in.
	forbidCmd bool

	mu     sync.Mutex
	db     *sql.DB
	status Status
	open   func(driver, dsn string) (*sql.DB, error) // overridable for tests
	pingTO time.Duration
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
		driver:         driver,
		host:           host,
		port:           port,
		user:           cfg.User,
		password:       cfg.Password,
		database:       database,
		encrypt:        cfg.Encrypt,
		tlsMin:         cfg.TLSMin,
		trustCert:      cfg.TrustCert,
		hostnameInCert: cfg.HostnameInCert,
		forbidCmd:      cfg.ForbidCmd,
		status: Status{
			Configured: true,
			Driver:     driver,
			Host:       host,
			Port:       port,
			Database:   database,
			ReadOnly:   cfg.ForbidCmd,
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
// by issuing a Ping if the pool is currently considered open. When the
// pool was never opened (startup ping failed), Status attempts a lazy
// reconnect first — the 2026-09-15 G4 report showed validate_config
// replaying a stale "connection refused" forever after MySQL came up.
func (o *SQLOp) Status(ctx context.Context) Status {
	o.mu.Lock()
	hasDB := o.db != nil
	o.mu.Unlock()
	if !hasDB {
		_ = o.ensureOpen(ctx) // best-effort; records any error in status
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	// Keep the lock flag honest even for ops constructed directly in
	// tests (NewSQLOp already seeds it at build time).
	o.status.ReadOnly = o.forbidCmd
	if o.db != nil {
		pingCtx, cancel := context.WithTimeout(ctx, o.pingTO)
		start := time.Now()
		err := o.db.PingContext(pingCtx)
		cancel()
		o.status.LatencyMs = time.Since(start).Milliseconds()
		if err != nil {
			o.status.Open = false
			o.markLocked(err)
			// Evict a pool which failed its health check. Leaving it in place
			// makes the next operation borrow the same dead connection before
			// retry logic gets a chance to rebuild the pool.
			_ = o.db.Close()
			o.db = nil
			return o.status
		}
		o.status.Open = true
	}
	return o.status
}

// ReadOnly reports whether this connector is locked read-only by
// config.json forbid_cmd. Pure accessor — used by the tool-description
// builder to flag locked connectors in the advertised tool list, without
// pinging anything.
func (o *SQLOp) ReadOnly() bool { return o.forbidCmd }

// Query runs one SELECT statement with row-size + row-count safety.
// On a forbid_cmd connector every statement must additionally pass
// checkReadOnlyQuery — the router's WITH / VALUES allowance is a
// smuggling vector there (WITH … DELETE is valid MySQL 8 / SQL Server).
func (o *SQLOp) Query(ctx context.Context, sqlText string, maxRows int) (QueryResult, error) {
	if o.forbidCmd {
		// Policy rejection: deliberately not recorded in status.LastError
		// (the connector itself is fine) and not retried.
		if err := checkReadOnlyQuery(sqlText); err != nil {
			return QueryResult{}, err
		}
	}
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
		_ = o.Close()
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

// Exec runs one non-SELECT statement. On a forbid_cmd connector it is
// refused outright, before any connection is borrowed — a locked
// connector must not be able to reach the database with DML / DDL at
// all, no matter how the statement was routed.
func (o *SQLOp) Exec(ctx context.Context, sqlText string) (ExecResult, error) {
	if o.forbidCmd {
		return ExecResult{}, ErrForbidCmd
	}
	er, err := o.runExec(ctx, sqlText)
	if err != nil && isRetryable(err) {
		// Never replay a write automatically: the server may have committed
		// it even when the response was lost. Drop the suspect pool so the
		// next explicit call reconnects, but return the uncertainty to the
		// operator instead of risking duplicate DML.
		_ = o.Close()
		err = fmt.Errorf("write failed on a stale connection and was not retried (commit status may be unknown): %w", err)
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
		Host:           o.host,
		Port:           o.port,
		User:           o.user,
		Password:       o.password,
		Database:       o.database,
		Encrypt:        o.encrypt,
		TLSMin:         o.tlsMin,
		TrustCert:      o.trustCert,
		HostnameInCert: o.hostnameInCert,
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
	db.SetConnMaxIdleTime(2 * time.Minute)
	db.SetConnMaxLifetime(5 * time.Minute)
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
	if schema, err := queryCurrentSchema(ctx, db, o.driver); err == nil {
		o.status.CurrentSchema = schema
	}
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

// dbHandle returns the live pool, lazily opening it when the startup
// ping failed or the pool was torn down. This is the 2026-09-15 G4① fix:
// previously Query/Exec returned "connector not open" forever when MySQL
// was not yet installed when the helper started.
func (o *SQLOp) dbHandle(ctx context.Context) (*sql.DB, error) {
	o.mu.Lock()
	db := o.db
	o.mu.Unlock()
	if db != nil {
		return db, nil
	}
	if err := o.ensureOpen(ctx); err != nil {
		return nil, err
	}
	o.mu.Lock()
	db = o.db
	o.mu.Unlock()
	return db, nil
}

// queryCurrentSchema reports the session's effective schema. In Oracle the
// configured Database is a service name and commonly differs from the schema
// where unqualified table names resolve.
func queryCurrentSchema(ctx context.Context, db *sql.DB, driver string) (string, error) {
	var query string
	switch driver {
	case "oracle":
		query = "SELECT SYS_CONTEXT('USERENV','CURRENT_SCHEMA') FROM DUAL"
	case "sqlserver", "mssql":
		query = "SELECT SCHEMA_NAME()"
	case "mysql":
		query = "SELECT DATABASE()"
	default:
		return "", nil
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var schema sql.NullString
	if err := db.QueryRowContext(qctx, query).Scan(&schema); err != nil {
		return "", err
	}
	if !schema.Valid {
		return "", nil
	}
	return schema.String, nil
}

func (o *SQLOp) runQuery(ctx context.Context, sqlText string, maxRows int) (QueryResult, error) {
	db, err := o.dbHandle(ctx)
	if err != nil {
		return QueryResult{}, err
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
			drained := 0
			for rows.Next() {
				drained++
				if drained >= maxRows*3 {
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
	db, err := o.dbHandle(ctx)
	if err != nil {
		return ExecResult{}, err
	}
	start := time.Now()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return ExecResult{}, fmt.Errorf("begin tx: %w", err)
	}
	res, err := tx.ExecContext(ctx, sqlText)
	if err != nil {
		_ = tx.Rollback()
		return ExecResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ExecResult{}, fmt.Errorf("commit: %w", err)
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

// buildDSN constructs the driver-specific connection string. Escaping is
// driver-specific: MySQL's DSN is not a URL, while Oracle and SQL Server are.
func buildDSN(driver string, cfg *config.DBConfig) (string, error) {
	port := cfg.EffectivePort(driver)
	switch driver {
	case "mysql":
		// go-sql-driver/mysql DSN: user:pass@tcp(host:port)/db?...
		// ParseDSN uses the first ':' for user/password and the last '@'
		// before tcp(...) for password/network. It performs no URL decoding,
		// so password bytes (including ':' and '@') must remain unchanged.
		user := cfg.User
		pass := cfg.Password
		host := cfg.Host
		if port > 0 {
			host = fmt.Sprintf("%s:%d", cfg.Host, port)
		}
		return fmt.Sprintf("%s:%s@tcp(%s)/%s?parseTime=true&charset=utf8mb4&timeout=10s&readTimeout=30s&writeTimeout=30s",
			user, pass, host, cfg.Database), nil
	case "sqlserver", "mssql":
		// microsoft/go-mssqldb DSN, URL form: sqlserver://user:pass@host:port?k=v
		// The driver's ADO parser (used for prefix-less strings) splits on ';'
		// only, so a bare url.Values.Encode() query string collapses into one
		// giant key and NOTHING — not even server/user/password/database —
		// reaches the driver. 2026-10-10 yangwuji: a TLS1.0-only SQL Server
		// produced "server selected unsupported protocol version 301" no
		// matter what encrypt/tls_min were set, because the whole parameter
		// set was swallowed; the ADO fallback then dialled localhost, which
		// is why it still managed to touch 127.0.0.1:1433 at all.
		userinfo := url.UserPassword(cfg.User, cfg.Password).String()
		host := cfg.Host
		if port > 0 {
			host = fmt.Sprintf("%s:%d", cfg.Host, port)
		}
		q := url.Values{}
		q.Set("database", cfg.Database)
		q.Set("dial timeout", "10")
		q.Set("connection timeout", "10")
		if v := strings.TrimSpace(cfg.Encrypt); v != "" {
			q.Set("encrypt", v)
		}
		if v := strings.TrimSpace(cfg.TLSMin); v != "" {
			q.Set("tlsmin", v)
		}
		if cfg.TrustCert {
			q.Set("trustservercertificate", "true")
		}
		if v := strings.TrimSpace(cfg.HostnameInCert); v != "" {
			q.Set("hostnameincertificate", v)
		}
		return fmt.Sprintf("sqlserver://%s@%s?%s", userinfo, host, q.Encode()), nil
	case "oracle":
		// sijms/go-ora/v2 DSN: oracle://user:pass@host:port/service_name
		// Use URL userinfo escaping so ?, #, /, spaces, @ and : cannot alter
		// the URL structure. This differs from MySQL's hand-written parser.
		userinfo := url.UserPassword(cfg.User, cfg.Password).String()
		host := cfg.Host
		if port > 0 {
			host = fmt.Sprintf("%s:%d", cfg.Host, port)
		}
		return fmt.Sprintf("oracle://%s@%s/%s", userinfo, host, cfg.Database), nil
	}
	return "", fmt.Errorf("unsupported driver %q", driver)
}
