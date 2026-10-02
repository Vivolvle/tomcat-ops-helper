package helpergui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tomcat-ops-helper/internal/config"
)

func writeCfg(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const sampleCfg = `{
  "_comment_": "keep me",
  "future_field": {"a": 1},
  "server_url": "wss://old.example.com/ws",
  "token": "abcdefghijklmnop",
  "tomcat_home": "C:\\tomcat",
  "jdk_home": "C:\\jdk",
  "log_dir": "C:\\tomcat\\logs",
  "allowed_dirs": ["C:\\tomcat"],
  "esb": {"host": "h", "user": "u", "password": "p", "database": "d"}
}`

// TestConfigStoreSavePreservesUnknownFields: unknown top-level fields
// (and agent edits made while the page was open) must survive a GUI
// save, while known fields are replaced and the result stays loadable.
func TestConfigStoreSavePreservesUnknownFields(t *testing.T) {
	p := writeCfg(t, t.TempDir(), sampleCfg)
	cs := &ConfigStore{Path: p}

	_, err := cs.Save(map[string]any{
		"server_url": "wss://new.example.com/ws",
		"esb":        nil, // delete the block
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	raw, _ := os.ReadFile(p)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("saved file not JSON: %v", err)
	}
	if doc["_comment_"] != "keep me" {
		t.Fatalf("_comment_ lost: %v", doc["_comment_"])
	}
	if _, ok := doc["future_field"]; !ok {
		t.Fatal("future_field lost")
	}
	if doc["server_url"] != "wss://new.example.com/ws" {
		t.Fatalf("server_url not replaced: %v", doc["server_url"])
	}
	if _, ok := doc["esb"]; ok {
		t.Fatal("esb:null must delete the block")
	}
	if strings.Contains(string(raw), ".tmp") {
		t.Fatal("tmp debris written into the config?")
	}
	if _, err := config.LoadHelper(p); err != nil {
		t.Fatalf("saved config must pass LoadHelper: %v", err)
	}
}

// TestConfigSaveRejectsInvalidKeepsOriginal: a validation failure must
// leave the original file byte-identical and no tmp file behind.
func TestConfigSaveRejectsInvalidKeepsOriginal(t *testing.T) {
	dir := t.TempDir()
	p := writeCfg(t, dir, sampleCfg)
	before, _ := os.ReadFile(p)
	cs := &ConfigStore{Path: p}

	if _, err := cs.Save(map[string]any{"token": "short"}); err == nil {
		t.Fatal("expected validation error for short token")
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Fatalf("original file must be untouched, got:\n%s", after)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("tmp debris left behind: %s", e.Name())
		}
	}
}

// TestConfigSaveFlagsRestart: only wsclient-captured fields set the
// restart hint.
func TestConfigSaveFlagsRestart(t *testing.T) {
	p := writeCfg(t, t.TempDir(), sampleCfg)
	cs := &ConfigStore{Path: p}

	if rr, err := cs.Save(map[string]any{"tomcat_home": `C:\tomcat10`}); err != nil || rr {
		t.Fatalf("path change must not require restart (rr=%v err=%v)", rr, err)
	}
	if rr, err := cs.Save(map[string]any{"token": "zyxwvutsrqponmlk"}); err != nil || !rr {
		t.Fatalf("token change must require restart (rr=%v err=%v)", rr, err)
	}
}

// TestDefaultDocIsLoadable: the fresh-install template must survive the
// helper's own loader once the placeholders are filled.
func TestDefaultDocIsLoadable(t *testing.T) {
	doc := defaultDoc()
	doc["token"] = "abcdefghijklmnop"
	doc["allowed_dirs"] = []any{`C:\tomcat`}
	b, _ := json.Marshal(doc)
	p := writeCfg(t, t.TempDir(), string(b))
	if _, err := config.LoadHelper(p); err != nil {
		t.Fatalf("template must validate: %v", err)
	}
}
