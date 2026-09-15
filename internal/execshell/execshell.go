// Package execshell implements the controlled shell action.
//
// All side-effects go through the Runner interface so unit tests can substitute
// a fake without spawning real processes.
package execshell

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"tomcat-ops-helper/internal/config"
)

// Runner abstracts command execution. The production runner uses
// os/exec; tests inject a fake.
type Runner interface {
	Run(ctx context.Context, name string, args []string, workDir string) (Result, error)
}

// Result captures everything we care about from a single invocation.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Took     time.Duration
	Truncated bool
}

// Auditor receives one entry per execution. It can be nil to disable auditing.
type Auditor func(entry AuditEntry)

// AuditEntry is what an Auditor typically serialises.
type AuditEntry struct {
	Timestamp  time.Time `json:"timestamp"`
	Cmd        string    `json:"cmd"`
	WorkDir    string    `json:"work_dir"`
	ExitCode   int       `json:"exit_code"`
	TookMs     int64     `json:"took_ms"`
	OutputBytes int      `json:"output_bytes"`
	Truncated  bool      `json:"truncated"`
	Error      string    `json:"error,omitempty"`
}

// blacklist contains patterns we refuse to run regardless of context.
var blacklist = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\brm\s+-rf\s+/`),
	regexp.MustCompile(`(?i)\bformat\b`),
	regexp.MustCompile(`(?i)\bshutdown\b`),
	regexp.MustCompile(`(?i)\bcurl\b[^\n|]*\|\s*bash\b`),
	regexp.MustCompile(`(?i)\bwget\b[^\n|]*\|\s*bash\b`),
	regexp.MustCompile(`(?i)\bdel\s+/f\s+/s\s+/q\s+([a-zA-Z]:\\|\\)`),
	regexp.MustCompile(`(?i)\brd\s+/s\s+/q\s+([a-zA-Z]:\\|\\)`),
	regexp.MustCompile(`(?i)\bnet\s+user\b`),
	regexp.MustCompile(`(?i)\bnet\s+localgroup\b`),
	regexp.MustCompile(`(?i)\breg\s+delete\b`),
	regexp.MustCompile(`(?i)\bbcdedit\b`),
}

// IsBlacklisted reports whether the command matches any deny pattern.
func IsBlacklisted(cmd string) (string, bool) {
	for _, re := range blacklist {
		if re.MatchString(cmd) {
			return re.String(), true
		}
	}
	return "", false
}

// ExtractPaths returns every absolute-looking path mentioned in the command,
// using a coarse heuristic suitable for guard-rail checks. It is intentionally
// conservative (may under-report) rather than permissive.
//
// Quoting is respected: "D:\Program Files\MySQL\bin\mysqld.exe" is one
// path, not two. The pre-2026-09-15 tokenizer split on every space and
// rejected such commands with "path not allowed: D:\Program" (todo G2).
// A quoted segment that itself contains a command (e.g.
// cmd /C "rmdir /S /Q E:\foo") is still scanned word-by-word so embedded
// paths are not missed.
//
// A token is treated as a path when it matches one of:
//
//   - drive-letter absolute path: `C:\...` or `C:/...`
//   - UNC absolute path: `\\server\share` or `//server/share`
//   - POSIX-style absolute path with at least two segments: `/foo/bar`
//     (a single-segment `/S` is a cmd.exe switch, not a path)
func ExtractPaths(cmd string) []string {
	out := []string{}
	seen := map[string]bool{}
	add := func(tok string) {
		tok = strings.TrimSpace(strings.Trim(tok, `"'`))
		if tok == "" || !looksLikePath(tok) {
			return
		}
		p := filepath.Clean(tok)
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, seg := range quotedSegments(cmd) {
		if looksLikePath(strings.TrimSpace(seg)) {
			add(seg)
			continue
		}
		for _, tok := range splitTokens(seg) {
			add(tok)
		}
	}
	for _, tok := range splitTokens(blankQuoted(cmd)) {
		add(tok)
	}
	return out
}

// CheckAllowedPaths returns ErrWorkDirNotAllowed when cmd mentions an
// absolute path outside cfg.AllowedDirs. Exposed for callers that
// transform the command before execution (e.g. the encoded PowerShell
// wrapper in the actions layer) so the guard rail still runs against the
// original, human-readable text.
func CheckAllowedPaths(cfg *config.HelperConfig, cmd string) error {
	for _, p := range ExtractPaths(cmd) {
		if !cfg.IsAllowedWorkDir(p) {
			return fmt.Errorf("%w: %s", ErrWorkDirNotAllowed, p)
		}
	}
	return nil
}

// quotedSegments returns the contents of every "..." pair, in order.
// Unterminated quotes are ignored (cmd.exe would reject them anyway).
func quotedSegments(s string) []string {
	var out []string
	for {
		i := strings.IndexByte(s, '"')
		if i < 0 {
			return out
		}
		rest := s[i+1:]
		j := strings.IndexByte(rest, '"')
		if j < 0 {
			return out
		}
		out = append(out, rest[:j])
		s = rest[j+1:]
	}
}

// blankQuoted replaces every quoted segment (quotes included) with
// spaces, so the unquoted pass never re-scans words the quoted pass
// already handled.
func blankQuoted(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' {
			inQuote = !inQuote
			b.WriteByte(' ')
			continue
		}
		if inQuote {
			b.WriteByte(' ')
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// splitTokens splits on whitespace and cmd.exe separators.
func splitTokens(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		switch r {
		case ' ', '\t', '\r', '\n', ';', '&', '|', '>', '<':
			return true
		}
		return false
	})
}

// looksLikePath applies the rules described on ExtractPaths.
func looksLikePath(tok string) bool {
	// Drive-letter: C:\ or C:/
	if len(tok) >= 3 && tok[1] == ':' && (tok[2] == '\\' || tok[2] == '/') {
		return true
	}
	// UNC: \\server or //server
	if strings.HasPrefix(tok, `\\`) || strings.HasPrefix(tok, `//`) {
		return true
	}
	// POSIX absolute: at least two slashes total so cmd.exe switches like
	// /S, /Q, /C don't get mistaken for paths.
	if strings.HasPrefix(tok, "/") && strings.Count(tok, "/") >= 2 {
		return true
	}
	return false
}

// Executor ties the pieces together: blacklist check, work-dir check, runner
// invocation, truncation, audit.
type Executor struct {
	cfg     *config.HelperConfig
	runner  Runner
	auditor Auditor
	now     func() time.Time
}

// NewExecutor builds an Executor with the default OS runner.
func NewExecutor(cfg *config.HelperConfig) *Executor {
	return &Executor{
		cfg:     cfg,
		runner:  NewOSRunner(),
		auditor: nil,
		now:     time.Now,
	}
}

// WithRunner swaps the runner. Used by tests.
func (e *Executor) WithRunner(r Runner) *Executor { e.runner = r; return e }

// WithAuditor wires an audit sink. Used by tests and production.
func (e *Executor) WithAuditor(a Auditor) *Executor { e.auditor = a; return e }

// ErrBlacklisted is returned when the command hits the deny list.
var ErrBlacklisted = errors.New("command blacklisted")

// ErrWorkDirNotAllowed is returned when work_dir is outside AllowedDirs.
var ErrWorkDirNotAllowed = errors.New("work dir not allowed")

// ErrEmpty is returned for an empty command.
var ErrEmpty = errors.New("empty command")

// Exec runs cmd in workDir, enforcing guard rails and audit logging.
func (e *Executor) Exec(ctx context.Context, cmd, workDir string) (Result, error) {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return Result{}, ErrEmpty
	}
	if pattern, hit := IsBlacklisted(cmd); hit {
		e.audit(cmd, workDir, Result{ExitCode: -1}, time.Since(e.now()), "blacklisted: "+pattern)
		return Result{}, fmt.Errorf("%w (matched %s)", ErrBlacklisted, pattern)
	}
	if workDir == "" {
		return Result{}, ErrWorkDirNotAllowed
	}
	if !e.cfg.IsAllowedWorkDir(workDir) {
		e.audit(cmd, workDir, Result{ExitCode: -1}, 0, "work dir not allowed: "+workDir)
		return Result{}, fmt.Errorf("%w: %s", ErrWorkDirNotAllowed, workDir)
	}
	if err := CheckAllowedPaths(e.cfg, cmd); err != nil {
		e.audit(cmd, workDir, Result{ExitCode: -1}, 0, err.Error())
		return Result{}, err
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, e.cfg.CommandTimeout)
	defer cancel()

	name, args := splitCommand(cmd)
	started := e.now()
	raw, err := e.runner.Run(timeoutCtx, name, args, workDir)
	took := e.now().Sub(started)
	if err != nil {
		// Runner returned a timeout / context error; surface a sentinel.
		if errors.Is(timeoutCtx.Err(), context.DeadlineExceeded) {
			e.audit(cmd, workDir, Result{ExitCode: -1, Took: took}, took, "timeout")
			return Result{Took: took}, fmt.Errorf("exec timeout after %s", e.cfg.CommandTimeout)
		}
		e.audit(cmd, workDir, raw, took, err.Error())
		return raw, err
	}
	raw.Took = took
	if int64(len(raw.Stdout)+len(raw.Stderr)) > e.cfg.MaxOutputSize {
		keep := int(e.cfg.MaxOutputSize) / 2
		raw.Stdout = truncateMiddle(raw.Stdout, keep, keep, len(raw.Stdout))
		raw.Stderr = truncateMiddle(raw.Stderr, keep, keep, len(raw.Stderr))
		raw.Truncated = true
	}
	e.audit(cmd, workDir, raw, took, "")
	return raw, nil
}

func (e *Executor) audit(cmd, workDir string, r Result, took time.Duration, note string) {
	if e.auditor == nil {
		return
	}
	e.auditor(AuditEntry{
		Timestamp:   e.now(),
		Cmd:         cmd,
		WorkDir:     workDir,
		ExitCode:    r.ExitCode,
		TookMs:      took.Milliseconds(),
		OutputBytes: len(r.Stdout) + len(r.Stderr),
		Truncated:   r.Truncated,
		Error:       note,
	})
}

func truncateMiddle(s string, head, tail, original int) string {
	if original <= head+tail {
		return s
	}
	marker := fmt.Sprintf("\n...[truncated %d bytes]...\n", original-head-tail)
	return s[:head] + marker + s[len(s)-tail:]
}

// splitCommand picks the platform-correct shell and tokenises cmd. We avoid
// pulling in shell-specific parsing rules; this is a deliberate subset that
// covers the typical needs (cd <dir> && <tool> <args>).
func splitCommand(cmd string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd", []string{"/C", cmd}
	}
	return "sh", []string{"-c", cmd}
}
