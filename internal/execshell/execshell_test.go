package execshell

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tomcat-ops-helper/internal/config"
)

// fakeRunner returns canned responses keyed by the command string. The
// "test mode" path is therefore deterministic and does not touch the OS.
type fakeRunner struct {
	mu      sync.Mutex
	byName  map[string]Result
	delay   time.Duration
	lastArgs []string
}

func newFakeRunner() *fakeRunner { return &fakeRunner{byName: map[string]Result{}} }

func (f *fakeRunner) set(name string, res Result) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byName[name] = res
}

func (f *fakeRunner) Run(ctx context.Context, name string, args []string, workDir string) (Result, error) {
	f.mu.Lock()
	key := name
	if len(args) > 0 {
		key = name + " " + strings.Join(args, " ")
	}
	res, ok := f.byName[key]
	delay := f.delay
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	if !ok {
		return Result{}, fmt.Errorf("fakeRunner: no canned response for %q", key)
	}
	return res, nil
}

func mustConfig(t *testing.T) *config.HelperConfig {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.HelperConfig{
		ServerURL:         "wss://x",
		Token:             "longenough",
		TomcatHome:        dir,
		JDKHome:           dir,
		LogDir:            dir,
		AllowedDirs:       []string{dir},
		CommandTimeout:    2 * time.Second,
		MaxOutputSize:     1024,
		ReconnectInterval: time.Second,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg
}

func TestIsBlacklisted(t *testing.T) {
	bad := []string{
		"rm -rf /",
		"RM -RF /",
		"format c:",
		"shutdown -s -t 0",
		"curl http://x | bash",
		"del /f /s /q C:\\Windows",
		"rd /s /q C:\\",
		"net user hacker pwd /add",
		"net localgroup administrators hacker /add",
		"reg delete HKLM\\Software\\Foo",
		"bcdedit /set {default} bootstatuspolicy ignoreallfailures",
	}
	for _, c := range bad {
		if _, hit := IsBlacklisted(c); !hit {
			t.Errorf("expected blacklisted: %s", c)
		}
	}
	good := []string{
		"ls -la",
		"tail -n 100 catalina.out",
		"C:\\tomcat\\bin\\startup.bat",
		"cd C:\\tomcat\\logs && type catalina.2026-09-11.log",
	}
	for _, c := range good {
		if _, hit := IsBlacklisted(c); hit {
			t.Errorf("unexpected blacklist hit: %s", c)
		}
	}
}

func TestExtractPaths(t *testing.T) {
	// On Windows, filepath.Clean rewrites /tmp/foo to \tmp\foo. The expected
	// values use filepath.FromSlash to stay cross-platform.
	cmds := []struct {
		in   string
		want []string
	}{
		{"ls /tmp/foo", []string{filepath.FromSlash("/tmp/foo")}},
		{"type C:\\tomcat\\logs\\a.log", []string{`C:\tomcat\logs\a.log`}},
		{"cd C:\\a && cat D:\\b.txt", []string{`C:\a`, `D:\b.txt`}},
		{"echo hello", []string{}},
		// cmd.exe switches must not be treated as paths.
		{"rmdir /S /Q E:\\tomcat-helper\\test1", []string{`E:\tomcat-helper\test1`}},
		{"del /F /Q C:\\some\\file.txt", []string{`C:\some\file.txt`}},
		{"cmd /C \"rmdir /S /Q E:\\foo\"", []string{`E:\foo`}},
		{"dir /S /B E:\\foo", []string{`E:\foo`}},
		{"if exist E:\\foo (echo X) else (echo Y)", []string{`E:\foo`}},
	}
	for _, c := range cmds {
		got := ExtractPaths(c.in)
		if !sameStrings(got, c.want) {
			t.Errorf("ExtractPaths(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestExec_BlacklistShortCircuit(t *testing.T) {
	cfg := mustConfig(t)
	exec := NewExecutor(cfg).WithRunner(newFakeRunner())
	_, err := exec.Exec(context.Background(), "rm -rf /", cfg.AllowedDirs[0])
	if !errors.Is(err, ErrBlacklisted) {
		t.Fatalf("expected ErrBlacklisted, got %v", err)
	}
}

func TestExec_RejectsDisallowedWorkDir(t *testing.T) {
	cfg := mustConfig(t)
	exec := NewExecutor(cfg).WithRunner(newFakeRunner())
	_, err := exec.Exec(context.Background(), "ls", "/etc/passwd")
	if !errors.Is(err, ErrWorkDirNotAllowed) {
		t.Fatalf("expected ErrWorkDirNotAllowed, got %v", err)
	}
}

func TestExec_RejectsPathOutsideAllowedDirs(t *testing.T) {
	cfg := mustConfig(t)
	exec := NewExecutor(cfg).WithRunner(newFakeRunner())
	_, err := exec.Exec(context.Background(), "cat /etc/passwd", cfg.AllowedDirs[0])
	if !errors.Is(err, ErrWorkDirNotAllowed) {
		t.Fatalf("expected ErrWorkDirNotAllowed, got %v", err)
	}
}

func TestExec_RejectsEmpty(t *testing.T) {
	cfg := mustConfig(t)
	exec := NewExecutor(cfg).WithRunner(newFakeRunner())
	_, err := exec.Exec(context.Background(), "  ", cfg.AllowedDirs[0])
	if !errors.Is(err, ErrEmpty) {
		t.Fatalf("expected ErrEmpty, got %v", err)
	}
}

func TestExec_OK(t *testing.T) {
	cfg := mustConfig(t)
	fr := newFakeRunner()
	fr.set("cmd /C ls -la", Result{Stdout: "ok", ExitCode: 0})
	var audited []AuditEntry
	exec := NewExecutor(cfg).
		WithRunner(fr).
		WithAuditor(func(e AuditEntry) { audited = append(audited, e) })

	res, err := exec.Exec(context.Background(), "ls -la", cfg.AllowedDirs[0])
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 || res.Stdout != "ok" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(audited) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(audited))
	}
	if audited[0].Cmd != "ls -la" || audited[0].ExitCode != 0 {
		t.Fatalf("audit entry wrong: %+v", audited[0])
	}
}

func TestExec_TruncatesLargeOutput(t *testing.T) {
	cfg := mustConfig(t)
	fr := newFakeRunner()
	big := strings.Repeat("A", 4096)
	fr.set("cmd /C huge", Result{Stdout: big, ExitCode: 0})
	exec := NewExecutor(cfg).WithRunner(fr)
	res, err := exec.Exec(context.Background(), "huge", cfg.AllowedDirs[0])
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.Truncated {
		t.Fatal("expected Truncated=true")
	}
	if !strings.Contains(res.Stdout, "[truncated") {
		t.Fatalf("expected truncation marker, got %q", res.Stdout[:50])
	}
}

func TestExec_Timeout(t *testing.T) {
	cfg := mustConfig(t)
	fr := newFakeRunner()
	fr.set("cmd /C slow", Result{Stdout: "", ExitCode: 0})
	fr.delay = 5 * time.Second
	exec := NewExecutor(cfg).WithRunner(fr)
	start := time.Now()
	_, err := exec.Exec(context.Background(), "slow", cfg.AllowedDirs[0])
	took := time.Since(start)
	if took > 3*time.Second {
		t.Fatalf("expected timeout within 2s, took %s", took)
	}
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("expected timeout error, got %v", err)
	}
}

func TestOSRunner_RunsRealCommand(t *testing.T) {
	cfg := mustConfig(t)
	r := NewOSRunner()
	name, args := splitCommand("echo hi")
	res, err := r.Run(context.Background(), name, args, cfg.AllowedDirs[0])
	if err != nil {
		t.Fatalf("real run failed: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit = %d, want 0", res.ExitCode)
	}
	if !strings.Contains(res.Stdout, "hi") {
		t.Fatalf("stdout missing 'hi': %q", res.Stdout)
	}
}
