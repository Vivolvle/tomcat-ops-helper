package actions

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tomcat-ops-helper/internal/config"
	"tomcat-ops-helper/internal/dbop"
)

// closeTrackingOp wraps an Op so tests can assert CloseDB actually closed
// the connector it removed.
type closeTrackingOp struct {
	dbop.Op
	closed bool
}

func (c *closeTrackingOp) Close() error {
	c.closed = true
	return c.Op.Close()
}

// writeReloadConfig writes body to config.json under a fresh temp dir and
// returns the absolute path (mirrors how main passes the -config path).
func writeReloadConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// reloadCfgBody is marshalled into config.json bodies. Building it via
// json.Marshal (instead of string concatenation) keeps Windows path
// backslashes properly escaped.
type reloadCfgBody struct {
	ServerURL      string   `json:"server_url"`
	Token          string   `json:"token"`
	TomcatHome     string   `json:"tomcat_home"`
	JDKHome        string   `json:"jdk_home"`
	LogDir         string   `json:"log_dir"`
	AllowedDirs    []string `json:"allowed_dirs"`
	CommandTimeout int      `json:"command_timeout"`
}

func reloadConfigV1(t *testing.T, dir string) string {
	t.Helper()
	b, err := json.Marshal(reloadCfgBody{
		ServerURL: "wss://ops.example.com/ws", Token: "abcdefghijklmnop",
		TomcatHome: dir, JDKHome: dir, LogDir: filepath.Join(dir, "logs"),
		AllowedDirs: []string{`C:\reload-v1`, dir}, CommandTimeout: 15,
	})
	if err != nil {
		t.Fatalf("marshal v1: %v", err)
	}
	return string(b)
}

func reloadConfigV2(t *testing.T, dir string) string {
	t.Helper()
	b, err := json.Marshal(reloadCfgBody{
		ServerURL: "wss://ops.example.com/ws", Token: "abcdefghijklmnop",
		TomcatHome: dir, JDKHome: dir, LogDir: filepath.Join(dir, "logs2"),
		AllowedDirs: []string{`C:\reload-v2`, dir}, CommandTimeout: 15,
	})
	if err != nil {
		t.Fatalf("marshal v2: %v", err)
	}
	return string(b)
}

// TestReloadConfig_NoConfigPath: a Registry built without WithConfigPath
// (unit tests, or a hypothetical embedder) must return a clear error that
// says restart is the fallback — not a panic or silent success.
func TestReloadConfig_NoConfigPath(t *testing.T) {
	reg := NewRegistry(mustCfg(t), mustExec(t))
	_, err := reg.reloadConfig(context.Background())
	if err == nil {
		t.Fatal("expected error without config path")
	}
	if !strings.Contains(err.Error(), "restart") {
		t.Fatalf("error should point at restart fallback, got: %v", err)
	}
}

// TestReloadConfig_SwapsAndFiresHooks: happy path — fresh config lands in
// the shared pointer, both hooks fire, the reply embeds validation.
func TestReloadConfig_SwapsAndFiresHooks(t *testing.T) {
	dir := t.TempDir()
	path := writeReloadConfig(t, reloadConfigV1(t, dir))
	cfg, err := config.LoadHelper(path)
	if err != nil {
		t.Fatalf("load v1: %v", err)
	}

	reg := NewRegistry(cfg, mustExec(t)).WithConfigPath(path)
	dbReloaded, toolsPushed := false, false
	reg.SetDBReloader(func() { dbReloaded = true })
	reg.SetToolsChanged(func() error { toolsPushed = true; return nil })

	if err := os.WriteFile(path, []byte(reloadConfigV2(t, dir)), 0o600); err != nil {
		t.Fatalf("write v2: %v", err)
	}
	raw, err := reg.reloadConfig(context.Background())
	if err != nil {
		t.Fatalf("reloadConfig: %v", err)
	}

	if cfg.LogDir != filepath.Join(dir, "logs2") {
		t.Fatalf("cfg.LogDir not swapped, got %q", cfg.LogDir)
	}
	if len(cfg.AllowedDirs) == 0 || cfg.AllowedDirs[0] != `C:\reload-v2` {
		t.Fatalf("cfg.AllowedDirs not swapped, got %v", cfg.AllowedDirs)
	}
	if !dbReloaded {
		t.Fatal("dbReloader hook did not fire")
	}
	if !toolsPushed {
		t.Fatal("toolsChanged hook did not fire")
	}

	var got ReloadData
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	if !got.Reloaded || !got.ToolListPushed || got.ToolListPushErr != "" {
		t.Fatalf("reply flags wrong: %+v", got)
	}
	if len(got.Validation) == 0 {
		t.Fatal("reply should embed validation payload")
	}
	if got.ConfigLoadedAt == "" {
		t.Fatal("reply should carry config_loaded_at")
	}
}

// TestReloadConfig_BadConfigKeepsOld: a broken config.json aborts the
// reload and the running config is untouched.
func TestReloadConfig_BadConfigKeepsOld(t *testing.T) {
	dir := t.TempDir()
	path := writeReloadConfig(t, reloadConfigV1(t, dir))
	cfg, err := config.LoadHelper(path)
	if err != nil {
		t.Fatalf("load v1: %v", err)
	}
	reg := NewRegistry(cfg, mustExec(t)).WithConfigPath(path)
	fired := false
	reg.SetDBReloader(func() { fired = true })

	if err := os.WriteFile(path, []byte(`{"server_url": `), 0o600); err != nil {
		t.Fatalf("write bad config: %v", err)
	}
	_, err = reg.reloadConfig(context.Background())
	if err == nil {
		t.Fatal("expected reload error on malformed config")
	}
	if !strings.Contains(err.Error(), "running config unchanged") {
		t.Fatalf("error should state the running config survived, got: %v", err)
	}
	if cfg.LogDir != filepath.Join(dir, "logs") {
		t.Fatalf("running config must stay untouched, LogDir=%q", cfg.LogDir)
	}
	if fired {
		t.Fatal("dbReloader must not fire on a failed reload")
	}
}

// TestReloadConfig_PushFailureIsReportedNotFatal: when the ToolList push
// fails (e.g. momentarily disconnected), the reload still succeeded and
// the reply carries the reason.
func TestReloadConfig_PushFailureIsReportedNotFatal(t *testing.T) {
	dir := t.TempDir()
	path := writeReloadConfig(t, reloadConfigV1(t, dir))
	cfg, err := config.LoadHelper(path)
	if err != nil {
		t.Fatalf("load v1: %v", err)
	}
	reg := NewRegistry(cfg, mustExec(t)).WithConfigPath(path)
	reg.SetToolsChanged(func() error { return errors.New("not connected") })

	if err := os.WriteFile(path, []byte(reloadConfigV2(t, dir)), 0o600); err != nil {
		t.Fatalf("write v2: %v", err)
	}
	raw, err := reg.reloadConfig(context.Background())
	if err != nil {
		t.Fatalf("reload must succeed despite push failure: %v", err)
	}
	var got ReloadData
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Reloaded {
		t.Fatal("reload flag must be true")
	}
	if got.ToolListPushErr != "not connected" {
		t.Fatalf("push error must be reported, got %q", got.ToolListPushErr)
	}
}

// TestCloseDB_ClosesAndUnregisters: the reload path must release the old
// connector's pool, and a second CloseDB is a no-op.
func TestCloseDB_ClosesAndUnregisters(t *testing.T) {
	reg := NewRegistry(mustCfg(t), mustExec(t))
	op := &closeTrackingOp{Op: &dbop.FakeOp{}}
	reg.WithDB("esb_db_operator", op)

	if !hasTool(reg.Tools(), "esb_db_operator") {
		t.Fatal("tool expected before CloseDB")
	}
	if !reg.CloseDB("esb_db_operator") {
		t.Fatal("CloseDB should report present connector")
	}
	if !op.closed {
		t.Fatal("CloseDB must close the old connector")
	}
	if hasTool(reg.Tools(), "esb_db_operator") {
		t.Fatal("tool must disappear after CloseDB")
	}
	if reg.CloseDB("esb_db_operator") {
		t.Fatal("second CloseDB should report absent")
	}
}

func hasTool(tools []Tool, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return true
		}
	}
	return false
}
