package actions

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tomcat-ops-helper/internal/config"
	"tomcat-ops-helper/internal/execshell"
)

// fakeProbe is a plug-in for the OS Probe; tests can seed it with canned data.
type fakeProbe struct {
	procs []JavaProcess
	ports []Port
}

func (f *fakeProbe) JavaProcesses(context.Context) ([]JavaProcess, error) {
	return f.procs, nil
}
func (f *fakeProbe) ListeningPorts(context.Context) ([]Port, error) {
	return f.ports, nil
}

func mustExec(t *testing.T) *execshell.Executor {
	t.Helper()
	cfg := mustCfg(t)
	return execshell.NewExecutor(cfg).WithRunner(execshell.NewOSRunner())
}

func mustCfg(t *testing.T) *config.HelperConfig {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.HelperConfig{
		ServerURL:         "wss://x",
		Token:             "longenough",
		TomcatHome:        dir,
		JDKHome:           dir,
		LogDir:            filepath.Join(dir, "logs"),
		AllowedDirs:       []string{dir},
		CommandTimeout:    30 * time.Second,
		MaxOutputSize:     1024,
		ReconnectInterval: time.Second,
	}
	if err := os.MkdirAll(cfg.LogDir, 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg
}

func TestStatus_FiltersTomcatProcs(t *testing.T) {
	cfg := mustCfg(t)
	exec := mustExec(t)
	probe := &fakeProbe{
		procs: []JavaProcess{
			{PID: 1, Cmdline: "java -Dcatalina.home=/opt/tomcat org.apache.catalina.startup.Bootstrap"},
			{PID: 2, Cmdline: "java -jar random-app.jar"},
		},
		ports: []Port{{Proto: "tcp", Addr: "0.0.0.0:8080"}},
	}
	reg := NewRegistry(cfg, exec).WithProbe(probe)
	raw, err := reg.status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var got StatusData
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.JavaProcs) != 1 {
		t.Fatalf("expected 1 interesting proc, got %d", len(got.JavaProcs))
	}
	if got.JavaProcs[0].PID != 1 {
		t.Fatalf("wrong proc kept: %+v", got.JavaProcs)
	}
	if got.OS == "" {
		t.Fatal("OS should be set")
	}
}

func TestListLogs_RejectsDisallowedDir(t *testing.T) {
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t))
	_, err := reg.listLogs(mustMarshal(map[string]any{"dir": "C:\\Windows"}))
	if err == nil {
		t.Fatal("expected error for disallowed dir")
	}
}

func TestListLogs_OK(t *testing.T) {
	cfg := mustCfg(t)
	first := filepath.Join(cfg.LogDir, "a.log")
	second := filepath.Join(cfg.LogDir, "bbb.log")
	if err := os.WriteFile(first, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Sleep so the second file has a strictly later mtime. Without this the
	// sort can be unstable on fast filesystems and the test flaps.
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(second, []byte("yyy"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	reg := NewRegistry(cfg, mustExec(t))
	raw, err := reg.listLogs(nil)
	if err != nil {
		t.Fatalf("listLogs: %v", err)
	}
	var got struct {
		Dir   string     `json:"dir"`
		Files []LogFile  `json:"files"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Dir != cfg.LogDir {
		t.Fatalf("dir = %s, want %s", got.Dir, cfg.LogDir)
	}
	if len(got.Files) != 2 {
		t.Fatalf("expected 2 files, got %d (%+v)", len(got.Files), got.Files)
	}
	// Newest first: bbb.log, then a.log.
	if got.Files[0].Name != "bbb.log" || got.Files[0].Size != 3 {
		t.Fatalf("first file = %+v, want bbb.log size=3", got.Files[0])
	}
	if got.Files[1].Name != "a.log" || got.Files[1].Size != 1 {
		t.Fatalf("second file = %+v, want a.log size=1", got.Files[1])
	}
}

func TestTail_OK(t *testing.T) {
	cfg := mustCfg(t)
	path := filepath.Join(cfg.LogDir, "sample.log")
	lines := []string{"a", "b", "c", "d", "e"}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	reg := NewRegistry(cfg, mustExec(t))
	raw, err := reg.tail(mustMarshal(map[string]any{"file": path, "lines": float64(3)}))
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	var got TailData
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Lines != 3 || got.Body[0] != "c" || got.Body[2] != "e" {
		t.Fatalf("tail mismatch: %+v", got)
	}
}

func TestTail_RejectsMissingFile(t *testing.T) {
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t))
	_, err := reg.tail(mustMarshal(map[string]any{"file": ""}))
	if !errors.Is(err, ErrMissingField) {
		t.Fatalf("expected ErrMissingField, got %v", err)
	}
}

func TestTail_RejectsDisallowedDir(t *testing.T) {
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t))
	_, err := reg.tail(mustMarshal(map[string]any{"file": "C:\\Windows\\System32\\drivers\\etc\\hosts", "lines": float64(5)}))
	if err == nil {
		t.Fatal("expected error for disallowed dir")
	}
}

func TestReplaceJar_CopiesAndHashes(t *testing.T) {
	cfg := mustCfg(t)
	webapps := filepath.Join(cfg.TomcatHome, "webapps")
	if err := os.MkdirAll(webapps, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	src := filepath.Join(cfg.TomcatHome, "uploads", "app.jar")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(src, []byte("JAR-CONTENT"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	dst := filepath.Join(webapps, "app.jar")
	if err := os.WriteFile(dst, []byte("OLD-JAR"), 0o644); err != nil {
		t.Fatalf("seed dest: %v", err)
	}

	reg := NewRegistry(cfg, mustExec(t))
	raw, err := reg.replaceJar(context.Background(), mustMarshal(map[string]any{"src": src, "dest": dst, "restart": false}))
	if err != nil {
		t.Fatalf("replaceJar: %v", err)
	}
	var got ReplaceJarData
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.SHA256 == "" || got.Bytes != int64(len("JAR-CONTENT")) {
		t.Fatalf("unexpected data: %+v", got)
	}
	bak := dst + ".bak"
	if _, err := os.Stat(bak); err != nil {
		t.Fatalf("expected backup file at %s: %v", bak, err)
	}
	body, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(body) != "JAR-CONTENT" {
		t.Fatalf("dst content wrong: %q", body)
	}
}

func TestReplaceJar_RejectsDisallowedDest(t *testing.T) {
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t))
	_, err := reg.replaceJar(context.Background(), mustMarshal(map[string]any{
		"src":  filepath.Join(cfg.AllowedDirs[0], "x.jar"),
		"dest": "C:\\Windows\\System32\\evil.exe",
	}))
	if err == nil {
		t.Fatal("expected error for disallowed dest")
	}
}

func TestKillZombie_ByPID(t *testing.T) {
	cfg := mustCfg(t)
	probe := &fakeProbe{procs: []JavaProcess{
		{PID: 99991, Cmdline: "java ... org.apache.catalina.startup.Bootstrap ..."},
		{PID: 99992, Cmdline: "java ... org.apache.catalina.startup.Bootstrap ..."},
	}}
	reg := NewRegistry(cfg, mustExec(t)).WithProbe(probe)
	raw, err := reg.killZombie(context.Background(), mustMarshal(map[string]any{"pid": float64(99991)}))
	if err != nil {
		t.Fatalf("killZombie: %v", err)
	}
	var got KillZombieData
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// We can't actually kill the fake PID in CI; allow either success or a
	// recorded error. What matters is the candidate set.
	if len(got.Found) != 1 || got.Found[0] != 99991 {
		t.Fatalf("expected only pid 99991 in Found, got %+v", got.Found)
	}
}

func TestKillZombie_ByPattern(t *testing.T) {
	cfg := mustCfg(t)
	probe := &fakeProbe{procs: []JavaProcess{
		{PID: 99993, Cmdline: "java -Dcatalina.home=foo"},
		{PID: 99994, Cmdline: "java -Dcatalina.home=bar"},
	}}
	reg := NewRegistry(cfg, mustExec(t)).WithProbe(probe)
	raw, err := reg.killZombie(context.Background(), mustMarshal(map[string]any{"pattern": "bar"}))
	if err != nil {
		t.Fatalf("killZombie: %v", err)
	}
	var got KillZombieData
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Found) != 1 || got.Found[0] != 99994 {
		t.Fatalf("expected pid 99994 found, got %+v", got.Found)
	}
}

func TestRegenService_WritesFile(t *testing.T) {
	cfg := mustCfg(t)
	bin := filepath.Join(cfg.TomcatHome, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	reg := NewRegistry(cfg, mustExec(t))
	raw, err := reg.regenService(nil)
	if err != nil {
		t.Fatalf("regenService: %v", err)
	}
	var got RegenServiceData
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Bytes <= 0 || got.Path == "" {
		t.Fatalf("unexpected response: %+v", got)
	}
	if _, err := os.Stat(got.Path); err != nil {
		t.Fatalf("expected file at %s: %v", got.Path, err)
	}
}

func TestDispatch_UnknownAction(t *testing.T) {
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t))
	_, err := reg.Dispatch(context.Background(), "nope", nil)
	if err == nil {
		t.Fatal("expected error for unknown action")
	}
}

func TestWriteFile_OK(t *testing.T) {
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t))
	dst := filepath.Join(cfg.LogDir, "note.txt")
	body := "床前明月光，疑是地上霜。\n举头望明月，低头思故乡。\n"

	raw, err := reg.writeFile(mustMarshal(map[string]any{
		"path": dst, "text": body,
	}))
	if err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	var got WriteFileData
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Path != dst {
		t.Fatalf("path = %s, want %s", got.Path, dst)
	}
	if got.Bytes != len(body) {
		t.Fatalf("bytes = %d, want %d", got.Bytes, len(body))
	}
	if got.Overwritten {
		t.Fatalf("overwritten should be false on first write")
	}

	onDisk, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	// UTF-8 no BOM: bytes match exactly and start with the Chinese character,
	// not the U+FEFF BOM.
	if string(onDisk) != body {
		t.Fatalf("readback mismatch:\nwant %q\ngot  %q", body, onDisk)
	}
	if len(onDisk) >= 3 && onDisk[0] == 0xEF && onDisk[1] == 0xBB && onDisk[2] == 0xBF {
		t.Fatalf("file starts with UTF-8 BOM, expected no BOM")
	}

	// Tmp file must be cleaned up after the atomic rename.
	if _, err := os.Stat(dst + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf(".tmp leftover: %v", err)
	}
}

func TestWriteFile_RejectsExistingWithoutOverwrite(t *testing.T) {
	cfg := mustCfg(t)
	dst := filepath.Join(cfg.LogDir, "present.txt")
	if err := os.WriteFile(dst, []byte("OLD"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	reg := NewRegistry(cfg, mustExec(t))
	_, err := reg.writeFile(mustMarshal(map[string]any{
		"path": dst, "text": "NEW",
	}))
	if !errors.Is(err, ErrFileExists) {
		t.Fatalf("expected ErrFileExists, got %v", err)
	}
	// Old content must still be on disk.
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	if string(got) != "OLD" {
		t.Fatalf("file changed unexpectedly: %q", got)
	}
}

func TestWriteFile_OverwriteTrueReplaces(t *testing.T) {
	cfg := mustCfg(t)
	dst := filepath.Join(cfg.LogDir, "replace.txt")
	if err := os.WriteFile(dst, []byte("OLD"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	reg := NewRegistry(cfg, mustExec(t))
	raw, err := reg.writeFile(mustMarshal(map[string]any{
		"path": dst, "text": "NEW", "overwrite": true,
	}))
	if err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	var got WriteFileData
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Overwritten {
		t.Fatalf("overwritten flag should be true")
	}
	onDisk, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	if string(onDisk) != "NEW" {
		t.Fatalf("file content = %q, want NEW", onDisk)
	}
}

func TestWriteFile_RejectsDisallowedPath(t *testing.T) {
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t))
	_, err := reg.writeFile(mustMarshal(map[string]any{
		"path": "C:\\Windows\\System32\\drivers\\etc\\hosts",
		"text": "boom",
	}))
	if err == nil {
		t.Fatal("expected error for disallowed path")
	}
}

func TestWriteFile_RequiresFields(t *testing.T) {
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t))
	cases := []map[string]any{
		{"text": "x"},                                 // missing path
		{"path": filepath.Join(cfg.LogDir, "no.txt")}, // missing text
	}
	for i, p := range cases {
		_, err := reg.writeFile(mustMarshal(p))
		if !errors.Is(err, ErrMissingField) {
			t.Fatalf("case %d: expected ErrMissingField, got %v", i, err)
		}
	}
}

func TestWriteFile_SHA256MatchesBody(t *testing.T) {
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t))
	dst := filepath.Join(cfg.LogDir, "hashed.txt")
	body := "the quick brown fox"
	raw, err := reg.writeFile(mustMarshal(map[string]any{"path": dst, "text": body}))
	if err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	var got WriteFileData
	_ = json.Unmarshal(raw, &got)
	want := sha256OfInline(body)
	if got.SHA256 != want {
		t.Fatalf("sha256 = %s, want %s", got.SHA256, want)
	}
}

// sha256OfInline is duplicated from probe.go's sha256Of to avoid exporting
// it from the package; the test only needs the lowercase hex digest of a
// known string.
func sha256OfInline(s string) string {
	h := newHash()
	h.Write([]byte(s))
	return hexOf(h)
}
