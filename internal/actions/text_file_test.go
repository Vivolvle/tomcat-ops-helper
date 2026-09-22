package actions

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"tomcat-ops-helper/internal/execshell"
)

func TestTextFileRoundTrip(t *testing.T) {
	cfg := mustCfg(t)
	r := NewRegistry(cfg, execshell.NewExecutor(cfg))
	path := filepath.Join(cfg.AllowedDirs[0], "server.xml")
	original := "\ufeff<Server port=\"8005\">中文</Server>\r\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	call := func(name string, p map[string]any) (json.RawMessage, error) {
		b, _ := json.Marshal(p)
		return r.Dispatch(context.Background(), name, b)
	}
	data, err := call("read_file", map[string]any{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	var read struct {
		Text   string
		SHA256 string
	}
	if err = json.Unmarshal(data, &read); err != nil {
		t.Fatal(err)
	}
	if read.Text != original {
		t.Fatal("read changed text")
	}
	p := map[string]any{"path": path, "old_text": "8005", "new_text": "8065", "expected_sha256": read.SHA256}
	if _, err = call("replace_text", p); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != strings.ReplaceAll(original, "8005", "8065") {
		t.Fatal("replacement changed unrelated bytes")
	}
	if _, err = call("replace_text", p); err == nil {
		t.Fatal("stale hash accepted")
	}
	p = map[string]any{"path": path, "old_text": "e", "new_text": "", "expected_count": 1}
	if _, err = call("replace_text", p); err == nil {
		t.Fatal("ambiguous replacement accepted")
	}
	p = map[string]any{"path": filepath.Join(t.TempDir(), "outside")}
	if _, err = call("read_file", p); err == nil {
		t.Fatal("outside path accepted")
	}
}
