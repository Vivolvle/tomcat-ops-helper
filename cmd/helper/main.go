// helper is the customer-side process: it loads config.json from its own
// directory, opens a reverse wss connection, and dispatches commands received
// from the operator.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"tomcat-ops-helper/internal/actions"
	"tomcat-ops-helper/internal/config"
	"tomcat-ops-helper/internal/dbop"
	"tomcat-ops-helper/internal/execshell"
	"tomcat-ops-helper/internal/protocol"
	"tomcat-ops-helper/internal/wsclient"
)

const helperVersion = "0.1.0"

func main() {
	var (
		configPath = flag.String("config", "config.json", "path to config.json")
		insecure   = flag.Bool("insecure-skip-verify", false, "skip TLS verification (test only)")
		quiet      = flag.Bool("quiet", false, "suppress non-error logs")
	)
	flag.Parse()

	if *configPath == "" {
		fatal(fmt.Errorf("config path is empty"))
	}
	abs, err := filepath.Abs(*configPath)
	if err != nil {
		fatal(fmt.Errorf("resolve config path: %w", err))
	}
	cfg, err := config.LoadHelper(abs)
	if err != nil {
		fatal(fmt.Errorf("load config: %w", err))
	}

	logger := log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds)
	if *quiet {
		logger.SetOutput(os.Stderr)
	}
	logger.Printf("helper %s starting; server=%s", helperVersion, cfg.ServerURL)
	logStartupWarnings(logger, cfg)

	// Audit log: append JSONL entries per executed command.
	auditPath := cfg.LogFile
	if !filepath.IsAbs(auditPath) {
		auditPath = filepath.Join(filepath.Dir(abs), auditPath)
	}
	auditFile, err := os.OpenFile(auditPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fatal(fmt.Errorf("open audit log: %w", err))
	}
	defer auditFile.Close()
	audit := func(e execshell.AuditEntry) {
		b, err := json.Marshal(e)
		if err != nil {
			return
		}
		b = append(b, '\n')
		_, _ = auditFile.Write(b)
	}

	exec := execshell.NewExecutor(cfg).WithAuditor(audit)
	registry := actions.NewRegistry(cfg, exec)

	hostname, _ := os.Hostname()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Database connectors. Eagerly opened at startup; failure is logged
	// as a warning rather than fatal so the helper still serves its
	// primary purpose (Tomcat / log / exec) even when the DB is
	// unreachable. The corresponding tool simply does not show up in
	// Tools() and dispatching it returns a clear "not configured".
	if cfg.ESB != nil {
		wireDB(ctx, logger, registry, "esb_db_operator", cfg.ESB)
	}
	if cfg.ERP != nil {
		wireDB(ctx, logger, registry, "erp_db_operator", cfg.ERP)
	}

	// Handle Ctrl+C / task-manager kill: stop the client cleanly.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		s := <-sigs
		logger.Printf("helper: signal %s received, shutting down", s)
		cancel()
	}()

	client, err := wsclient.New(wsclient.Options{
		URL:                cfg.ServerURL,
		Token:              cfg.Token,
		InsecureSkipVerify: *insecure || cfg.InsecureSkipVerify,
		HeartbeatInterval:  cfg.HeartbeatInterval,
		HeartbeatTimeout:   cfg.HeartbeatTimeout,
		ReconnectInterval:  cfg.ReconnectInterval,
		Logger:             logger,
		Hello: func() protocol.Hello {
			clientID := cfg.ClientID
			if clientID == "" {
				clientID = "helper-" + hostname
			}
			return protocol.Hello{
				ClientID: clientID,
				Token:    cfg.Token,
				Version:  helperVersion,
				Hostname: hostname,
				SentAt:   time.Now().UnixMilli(),
			}
		},
		PostConnect: func(conn interface {
			WriteJSON(v any) error
		}) error {
			tools := registry.Tools()
			meta := make([]protocol.ToolMeta, 0, len(tools))
			for _, t := range tools {
				meta = append(meta, protocol.ToolMeta{
					Name:        t.Name,
					Description: t.Description,
					InputSchema: t.InputSchema,
				})
			}
			return conn.WriteJSON(protocol.ToolList{Tools: meta}.ToEnvelope())
		},
		Handler: func(ctx context.Context, cmd protocol.Command) protocol.Result {
			return dispatch(ctx, registry, cmd)
		},
	})
	if err != nil {
		fatal(err)
	}

	if err := client.Run(ctx); err != nil && err != context.Canceled {
		fatal(err)
	}
	logger.Printf("helper: stopped")
}

// logStartupWarnings surfaces missing/invalid configured paths right in
// the helper log. The 2026-09-15 report (todo G1③) showed the operator
// guessing for six calls why exec kept failing: cfg.tomcat_home did not
// exist and was not in allowed_dirs, which only the deep error path
// mentioned.
func logStartupWarnings(logger *log.Logger, cfg *config.HelperConfig) {
	bad := 0
	check := func(label, path string) {
		st, err := os.Stat(path)
		switch {
		case err != nil:
			bad++
			logger.Printf("warn: %s not found: %s (%v)", label, path, err)
		case !st.IsDir():
			bad++
			logger.Printf("warn: %s is not a directory: %s", label, path)
		}
	}
	check("tomcat_home", cfg.TomcatHome)
	check("jdk_home", cfg.JDKHome)
	check("log_dir", cfg.LogDir)
	for _, d := range cfg.AllowedDirs {
		check("allowed_dirs entry", d)
	}
	if bad > 0 {
		logger.Printf("warn: %d configured path(s) missing or invalid; exec calls without work_dir fall back to the first existing allowed_dirs entry", bad)
	}
}

func dispatch(ctx context.Context, reg *actions.Registry, cmd protocol.Command) protocol.Result {
	started := time.Now()
	res := protocol.Result{ID: cmd.ID}
	data, err := reg.Dispatch(ctx, cmd.Action, cmd.Params)
	if err != nil {
		res.Success = false
		res.Error = err.Error()
	} else {
		res.Success = true
		res.Data = data
	}
	res.TookMs = time.Since(started).Milliseconds()
	return res
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "helper fatal:", err)
	os.Exit(1)
}

// wireDB opens a dbop.Op for the given toolName and registers it on
// the registry. Eager ping failure is logged as a warning — the Op is
// still registered so the tool shows up in Tools(), and dispatch will
// surface the failure on the first call (with a fresh retry attempt).
func wireDB(ctx context.Context, logger *log.Logger, reg *actions.Registry, toolName string, cfg *config.DBConfig) {
	driver := cfg.Driver()
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	op, err := dbop.NewSQLOp(pingCtx, cfg)
	if err != nil {
		logger.Printf("warn: %s (%s@%s:%d/%s): startup ping failed: %v — tool will retry on first use",
			toolName, cfg.User, cfg.Host, cfg.EffectivePort(driver), cfg.Database, err)
		// Still register the Op: it has the retry-once machinery and
		// will reconnect lazily when the operator actually uses the
		// tool. Otherwise the operator gets a confusing "not
		// configured" instead of a useful "connection refused".
		if op != nil {
			reg.WithDB(toolName, op)
		}
		return
	}
	logger.Printf("info: %s connected (%s@%s:%d/%s)",
		toolName, cfg.User, cfg.Host, cfg.EffectivePort(driver), cfg.Database)
	reg.WithDB(toolName, op)
}
