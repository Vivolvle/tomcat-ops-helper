// Package actions implements the named (non-exec) helper actions.
//
// Each action is a self-contained Handle method that takes JSON params and
// returns JSON data. Dependencies that touch the OS (process listing, file IO)
// go through small interfaces so unit tests can substitute fakes.
package actions

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf16"

	"tomcat-ops-helper/internal/config"
	"tomcat-ops-helper/internal/dbop"
	"tomcat-ops-helper/internal/execshell"
)

// Action is the contract every named action satisfies.
type Action interface {
	Name() string
	Handle(ctx context.Context, params json.RawMessage) (json.RawMessage, error)
}

// Registry routes a command action string to its handler.
type Registry struct {
	cfg      *config.HelperConfig
	executor *execshell.Executor
	probe    Probe
	uploads  *uploadRegistry
	dbops    map[string]dbop.Op // keyed by tool name ("esb_db_operator", "erp_db_operator")
}

// Probe abstracts OS-level introspection. Production uses OSProbe; tests
// substitute FakeProbe.
type Probe interface {
	JavaProcesses(ctx context.Context) ([]JavaProcess, error)
	ListeningPorts(ctx context.Context) ([]Port, error)
}

// JavaProcess describes a running JVM we care about.
type JavaProcess struct {
	PID     int    `json:"pid"`
	User    string `json:"user,omitempty"`
	Cmdline string `json:"cmdline"`
	Main    string `json:"main,omitempty"`
}

// Port is one TCP listener relevant to the helper.
type Port struct {
	Proto string `json:"proto"`
	Addr  string `json:"addr"`
	PID   int    `json:"pid,omitempty"`
}

// NewRegistry wires the default Registry.
func NewRegistry(cfg *config.HelperConfig, exec *execshell.Executor) *Registry {
	return &Registry{
		cfg:      cfg,
		executor: exec,
		probe:    NewOSProbe(),
		uploads:  newUploadRegistry(),
		dbops:    map[string]dbop.Op{},
	}
}

// WithProbe swaps the probe. Used by tests.
func (r *Registry) WithProbe(p Probe) *Registry { r.probe = p; return r }

// WithDB registers a database connector under a tool name. The tool
// only appears in Tools() after this is called for it; missing
// connectors simply make the corresponding tool unavailable.
func (r *Registry) WithDB(toolName string, op dbop.Op) *Registry {
	if op == nil {
		delete(r.dbops, toolName)
		return r
	}
	r.dbops[toolName] = op
	return r
}

// Dispatch routes an action by name.
func (r *Registry) Dispatch(ctx context.Context, name string, params json.RawMessage) (json.RawMessage, error) {
	switch name {
	case "status":
		return r.status(ctx)
	case "list_logs":
		return r.listLogs(params)
	case "tail":
		return r.tail(params)
	case "restart":
		return r.restart(ctx, params)
	case "replace_jar":
		return r.replaceJar(ctx, params)
	case "kill_zombie":
		return r.killZombie(ctx, params)
	case "regen_service":
		return r.regenService(params)
	case "exec":
		return r.execAction(ctx, params)
	case "write_file":
		return r.writeFile(params)
	case "append_text_file":
		return r.appendTextFile(params)
	case "upload_file":
		return r.uploadFile(params)
	case "validate_config":
		return r.validateConfig(ctx)
	case "esb_db_operator", "erp_db_operator":
		return r.dbOperator(ctx, name, params)
	default:
		return nil, fmt.Errorf("actions: unknown action %q", name)
	}
}

// execAction routes the "exec" action through the same Executor used by every
// other action, with the same work_dir / allowed_dirs enforcement.
func (r *Registry) execAction(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	p, err := decodeParams(params)
	if err != nil {
		return nil, err
	}
	cmdStr, err := requireString(p, "cmd")
	if err != nil {
		return nil, fmt.Errorf("exec: %w", err)
	}
	explicitWorkDir, _ := p["work_dir"].(string)
	workDir, fallbackFrom := r.resolveWorkDir(explicitWorkDir)

	// Optional shell override. By default we go through cmd /C on Windows
	// (or sh -c on Unix), which is fine for simple pipelines. When the
	// caller wants PowerShell-specific syntax (cmdlets, etc.),
	// "shell": "powershell" wraps the command in an encoded PowerShell
	// script with a `Set-Location` prologue so the work_dir still applies.
	if s, _ := p["shell"].(string); s != "" {
		switch strings.ToLower(s) {
		case "powershell", "pwsh":
			// The wrapper base64-encodes the command, so Executor.Exec's
			// path scan can no longer see the paths inside it. Run the
			// guard rail on the original text before wrapping.
			if err := execshell.CheckAllowedPaths(r.cfg, cmdStr); err != nil {
				return nil, fmt.Errorf("exec: %w", err)
			}
			cmdStr = powershellWrap(cmdStr, workDir)
		case "cmd":
			// explicit "cmd" = no-op; default already is cmd on Windows.
		case "sh":
			// no-op on Unix. On Windows this would fail; not silently rewritten.
		default:
			return nil, fmt.Errorf("exec: unknown shell %q (want powershell|pwsh|cmd|sh)", s)
		}
	}

	// Optional per-command timeout override. When set, wraps ctx so
	// Executor.Exec sees a tighter deadline than cfg.CommandTimeout.
	if v, ok := p["timeout"].(float64); ok && v > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(v*float64(time.Second)))
		defer cancel()
	}

	out, err := r.executor.Exec(ctx, cmdStr, workDir)
	fields := map[string]any{
		"stdout":    out.Stdout,
		"stderr":    out.Stderr,
		"exit_code": out.ExitCode,
		"took_ms":   out.Took.Milliseconds(),
		"truncated": out.Truncated,
		"work_dir":  workDir,
	}
	if fallbackFrom != "" {
		// Surface the substitution so the caller knows why commands are
		// running somewhere other than cfg.tomcat_home (todo G1②).
		fields["work_dir_fallback"] = true
		fields["work_dir_default"] = fallbackFrom
	}
	data := mustMarshal(fields)
	if err != nil {
		return data, err
	}
	return data, nil
}

// resolveWorkDir picks the working directory for exec. An explicit
// work_dir is passed through untouched (Executor validates it). When the
// caller omits it, cfg.tomcat_home is used — but only if it exists and
// is inside allowed_dirs. Otherwise the first existing allowed_dirs entry
// is used, so a stale tomcat_home cannot wedge every exec call with
// "work dir not allowed" (2026-09-15 G1②). fallbackFrom is non-empty when
// the configured default was unusable and carries the value it replaced.
func (r *Registry) resolveWorkDir(explicit string) (workDir, fallbackFrom string) {
	if strings.TrimSpace(explicit) != "" {
		return explicit, ""
	}
	def := r.cfg.TomcatHome
	if r.cfg.IsAllowedWorkDir(def) && dirExists(def) {
		return def, ""
	}
	for _, d := range r.cfg.AllowedDirs {
		if dirExists(d) {
			return d, def
		}
	}
	if len(r.cfg.AllowedDirs) > 0 {
		return r.cfg.AllowedDirs[0], def
	}
	return def, ""
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// powershellWrap turns cmdStr into `powershell -EncodedCommand <base64>`,
// where the script is `Set-Location -LiteralPath '<workDir>'; <cmdStr>`.
//
// Encoding as UTF-16LE base64 (the format -EncodedCommand expects) is
// deliberate: the 2026-09-15 customer-box report (todo G3) showed the old
// plain-text `-Command "…"` wrapper being echoed back instead of executed,
// because cmd.exe's quote handling ate the string. Base64 contains no
// quotes and no cmd.exe metacharacters, so nothing can mangle it.
func powershellWrap(cmdStr, workDir string) string {
	script := "Set-Location -LiteralPath '" +
		strings.ReplaceAll(workDir, "'", "''") + "'; " + cmdStr
	return "powershell -NoProfile -NonInteractive -EncodedCommand " +
		base64.StdEncoding.EncodeToString(utf16LEBytes(script))
}

// utf16LEBytes encodes s as UTF-16LE, the byte order PowerShell's
// -EncodedCommand expects.
func utf16LEBytes(s string) []byte {
	units := utf16.Encode([]rune(s))
	buf := make([]byte, 0, len(units)*2)
	for _, u := range units {
		buf = append(buf, byte(u), byte(u>>8))
	}
	return buf
}

// dbOperator runs a SQL script against the connector registered for
// toolName. The dispatch entry catches both "esb_db_operator" and
// "erp_db_operator" so the only difference between them is which Op
// the main package wired in.
//
// If no Op is registered for the tool, returns a clear "not
// configured" error — this happens when config.json omits the block
// (tool also won't show in Tools()).
func (r *Registry) dbOperator(ctx context.Context, toolName string, params json.RawMessage) (json.RawMessage, error) {
	op, ok := r.dbops[toolName]
	if !ok || op == nil {
		return nil, fmt.Errorf("%s: db connector not configured", toolName)
	}
	p, err := decodeParams(params)
	if err != nil {
		return nil, fmt.Errorf("%s: params: %w", toolName, err)
	}
	script, err := requireString(p, "sql")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", toolName, err)
	}
	maxRows := dbop.DefaultMaxRows
	if v, ok := p["max_rows"].(float64); ok && v > 0 {
		maxRows = int(v)
	}
	if maxRows > dbop.MaxRowsCap {
		maxRows = dbop.MaxRowsCap
	}
	perStmt := 30 * time.Second
	if v, ok := p["statement_timeout"].(float64); ok && v > 0 {
		perStmt = time.Duration(v * float64(time.Second))
	}
	total := 120 * time.Second
	if v, ok := p["timeout"].(float64); ok && v > 0 {
		total = time.Duration(v * float64(time.Second))
	}
	if total < perStmt {
		total = perStmt
	}
	res := dbop.RunScript(ctx, op, script, maxRows, perStmt, total)
	// Stamp the connector name for the audit log / UI to know which
	// DB a result came from even when many tools share schema.
	st := op.Status(ctx)
	res.Driver = st.Driver
	if res.Database == "" {
		res.Database = st.Database
	}
	return mustMarshal(res), nil
}

// ErrMissingField is returned when a required param is absent or empty.
var ErrMissingField = errors.New("missing required field")

func requireString(params map[string]any, key string) (string, error) {
	v, ok := params[key]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrMissingField, key)
	}
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("%w: %s must be non-empty string", ErrMissingField, key)
	}
	return s, nil
}

func decodeParams(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	m := map[string]any{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("params: %w", err)
	}
	return m, nil
}

func mustMarshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// sortedKeys returns a deterministic slice of string keys for stable logging.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
