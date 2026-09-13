package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tomcat-ops-helper/internal/actions"
	"tomcat-ops-helper/internal/config"
	"tomcat-ops-helper/internal/execshell"
	"tomcat-ops-helper/internal/protocol"
)

// helperFixture wires a minimal dispatcher the same way cmd/helper/main.go does.
type helperFixture struct {
	cfg      *config.HelperConfig
	exec     *execshell.Executor
	registry *actions.Registry
}

func newFixture(t *testing.T) *helperFixture {
	t.Helper()
	dir := t.TempDir()
	logs := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	cfg := &config.HelperConfig{
		ServerURL:         "wss://x",
		Token:             "longenough",
		TomcatHome:        dir,
		JDKHome:           dir,
		LogDir:            logs,
		AllowedDirs:       []string{dir},
		CommandTimeout:    2 * time.Second,
		MaxOutputSize:     1024,
		ReconnectInterval: time.Second,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	exec := execshell.NewExecutor(cfg)
	return &helperFixture{
		cfg:      cfg,
		exec:     exec,
		registry: actions.NewRegistry(cfg, exec),
	}
}

func TestDispatch_Status(t *testing.T) {
	f := newFixture(t)
	cmd := protocol.Command{ID: "1", Action: "status"}
	res := dispatch(context.Background(), f.registry, cmd)
	if !res.Success {
		t.Fatalf("status failed: %s", res.Error)
	}
	var got actions.StatusData
	if err := json.Unmarshal(res.Data, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.OS == "" {
		t.Fatal("OS missing")
	}
}

func TestDispatch_ExecEcho(t *testing.T) {
	f := newFixture(t)
	params := map[string]any{"cmd": "echo hello", "work_dir": f.cfg.AllowedDirs[0]}
	raw, _ := json.Marshal(params)
	cmd := protocol.Command{ID: "2", Action: "exec", Params: raw}
	res := dispatch(context.Background(), f.registry, cmd)
	if !res.Success {
		t.Fatalf("exec failed: %s", res.Error)
	}
	var got struct {
		Stdout   string `json:"stdout"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.Unmarshal(res.Data, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ExitCode != 0 {
		t.Fatalf("exit_code=%d, want 0", got.ExitCode)
	}
	if got.Stdout == "" || got.Stdout[:5] != "hello" {
		t.Fatalf("stdout wrong: %q", got.Stdout)
	}
}

func TestDispatch_ExecBlacklisted(t *testing.T) {
	f := newFixture(t)
	params := map[string]any{"cmd": "rm -rf /", "work_dir": f.cfg.AllowedDirs[0]}
	raw, _ := json.Marshal(params)
	cmd := protocol.Command{ID: "3", Action: "exec", Params: raw}
	res := dispatch(context.Background(), f.registry, cmd)
	if res.Success {
		t.Fatal("expected failure for blacklisted cmd")
	}
	if res.Error == "" {
		t.Fatal("expected error message")
	}
}

func TestDispatch_UnknownAction(t *testing.T) {
	f := newFixture(t)
	cmd := protocol.Command{ID: "4", Action: "no_such"}
	res := dispatch(context.Background(), f.registry, cmd)
	if res.Success {
		t.Fatal("expected failure for unknown action")
	}
}

func TestDispatch_ListLogs(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(filepath.Join(f.cfg.LogDir, "a.log"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cmd := protocol.Command{ID: "5", Action: "list_logs"}
	res := dispatch(context.Background(), f.registry, cmd)
	if !res.Success {
		t.Fatalf("list_logs failed: %s", res.Error)
	}
	if !strings.Contains(string(res.Data), `"a.log"`) {
		t.Fatalf("missing a.log in response: %s", res.Data)
	}
}
