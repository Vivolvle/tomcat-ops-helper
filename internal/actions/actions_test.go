package actions

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"tomcat-ops-helper/internal/config"
	"tomcat-ops-helper/internal/dbop"
	"tomcat-ops-helper/internal/execshell"
)

// fakeProbe is a plug-in for the OS Probe; tests can seed it with canned data.
type fakeProbe struct {
	procs    []JavaProcess
	ports    []Port
	javaErr  error
	portsErr error
}

type captureRunner struct {
	name string
	args []string
}

func (f *captureRunner) Run(_ context.Context, name string, args []string, _ string) (execshell.Result, error) {
	f.name = name
	f.args = append([]string(nil), args...)
	return execshell.Result{ExitCode: 0, Stdout: "terminated"}, nil
}

func (f *fakeProbe) JavaProcesses(context.Context) ([]JavaProcess, error) {
	return f.procs, f.javaErr
}
func (f *fakeProbe) ListeningPorts(context.Context) ([]Port, error) {
	return f.ports, f.portsErr
}

// scriptRule is one scripted response for scriptRunner. The first rule whose
// match substring appears in the full command line wins; stdouts is consumed
// one entry per hit (the last entry repeats) and failFirst makes the first N
// hits fail with exit 1 + stderr.
type scriptRule struct {
	match     string
	stdouts   []string
	stderr    string
	failFirst int
	hits      int
}

// scriptRunner fakes execshell.Runner for tests that drive the sc command
// family. It records every full command line for assertions.
type scriptRunner struct {
	calls []string
	rules []scriptRule
}

func (s *scriptRunner) Run(_ context.Context, name string, args []string, _ string) (execshell.Result, error) {
	full := name + " " + strings.Join(args, " ")
	s.calls = append(s.calls, full)
	for i := range s.rules {
		r := &s.rules[i]
		if !strings.Contains(full, r.match) {
			continue
		}
		r.hits++
		stdout := ""
		if len(r.stdouts) > 0 {
			idx := r.hits - 1
			if idx >= len(r.stdouts) {
				idx = len(r.stdouts) - 1
			}
			stdout = r.stdouts[idx]
		}
		if r.failFirst > 0 && r.hits <= r.failFirst {
			return execshell.Result{ExitCode: 1, Stdout: stdout, Stderr: r.stderr}, nil
		}
		return execshell.Result{ExitCode: 0, Stdout: stdout}, nil
	}
	return execshell.Result{ExitCode: 0}, nil
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
	cfg.LoadedAt = time.Now().Add(-2 * time.Minute) // pretend helper loaded 2 min ago
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
	// ConfigLoadedAt round-trips so the operator can compare it against
	// config.json's mtime on disk to detect a "edited but not restarted"
	// scenario (problem.md feedback ⑤).
	if got.ConfigLoadedAt.IsZero() {
		t.Fatal("ConfigLoadedAt should not be zero")
	}
	if d := time.Since(got.ConfigLoadedAt); d < time.Minute || d > 3*time.Minute {
		t.Fatalf("ConfigLoadedAt not preserved (delta=%s, want ~2m)", d)
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
		Dir   string    `json:"dir"`
		Files []LogFile `json:"files"`
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

// TestListLogs_IncludesDirs (todo H2): subdirectory entries must appear
// with type dir so callers can descend without another tool.
func TestListLogs_IncludesDirs(t *testing.T) {
	cfg := mustCfg(t)
	if err := os.Mkdir(filepath.Join(cfg.LogDir, "subdir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	reg := NewRegistry(cfg, mustExec(t))
	raw, err := reg.listLogs(nil)
	if err != nil {
		t.Fatalf("listLogs: %v", err)
	}
	var got struct {
		Files []LogFile `json:"files"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var dirEntry, fileTypesOK = false, true
	for _, f := range got.Files {
		switch {
		case f.Name == "subdir":
			dirEntry = f.Type == "dir"
		case f.Type != "file":
			fileTypesOK = false
		}
	}
	if !dirEntry {
		t.Fatalf("subdir must be listed with type dir, got %+v", got.Files)
	}
	if !fileTypesOK {
		t.Fatalf("non-dir entries must carry type file, got %+v", got.Files)
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

func TestAppendTextFile_CreatesAndAppendsUTF8(t *testing.T) {
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t))
	path := filepath.Join(cfg.TomcatHome, "scripts", "说明.ps1")
	for _, text := range []string{"第一行\n", "第二行\n"} {
		if _, err := reg.appendTextFile(mustMarshal(map[string]any{"path": path, "text": text})); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got, want := string(body), "第一行\n第二行\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
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

func TestKillPID_BuildsPlatformCommand(t *testing.T) {
	cfg := mustCfg(t)
	runner := &captureRunner{}
	reg := NewRegistry(cfg, execshell.NewExecutor(cfg).WithRunner(runner))
	raw, err := reg.killProcess(context.Background(), mustMarshal(map[string]any{"pid": float64(424242)}))
	if err != nil {
		t.Fatalf("killProcess: %v", err)
	}
	var got KillPIDData
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	joined := runner.name + " " + strings.Join(runner.args, " ")
	if runtime.GOOS == "windows" {
		for _, want := range []string{"taskkill", "/T", "/F", "/PID 424242"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("Windows kill command %q missing %q", joined, want)
			}
		}
		if !got.Tree || !got.Force {
			t.Fatalf("Windows defaults = tree:%v force:%v, want true/true", got.Tree, got.Force)
		}
	} else if !strings.Contains(joined, "kill -KILL 424242") {
		t.Fatalf("Unix kill command = %q", joined)
	}
}

func TestKillPID_RejectsHelperItself(t *testing.T) {
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t))
	_, err := reg.killProcess(context.Background(), mustMarshal(map[string]any{"pid": float64(os.Getpid())}))
	if err == nil || !strings.Contains(err.Error(), "helper process itself") {
		t.Fatalf("expected self-kill rejection, got %v", err)
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

func TestResolveServiceName_ConfiguredWins(t *testing.T) {
	if got := resolveServiceName("irrelevant", "MyService"); got != "MyService" {
		t.Fatalf("configured override ignored: %q", got)
	}
}

func TestResolveServiceName_FallsBackToConnectorPort(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "conf")
	if err := os.MkdirAll(conf, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	xml := `<?xml version="1.0"?>
<Server port="8005" shutdown="SHUTDOWN">
  <Service name="Catalina">
    <Connector port="8080" protocol="HTTP/1.1"/>
    <Connector port="8443" protocol="org.apache.coyote.http11.Http11NioProtocol"/>
  </Service>
</Server>
`
	if err := os.WriteFile(filepath.Join(conf, "server.xml"), []byte(xml), 0o644); err != nil {
		t.Fatalf("write server.xml: %v", err)
	}
	if got := resolveServiceName(dir, ""); got != "Tomcat8080" {
		t.Fatalf("expected Tomcat8080, got %q", got)
	}
}

func TestResolveServiceName_FallsBackToPlainTomcat(t *testing.T) {
	dir := t.TempDir() // no conf/server.xml
	if got := resolveServiceName(dir, ""); got != "Tomcat" {
		t.Fatalf("expected Tomcat fallback, got %q", got)
	}
}

func TestResolvePrunsrvSource_PrefersTomcat10(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"tomcat10.exe", "tomcat10w.exe", "tomcat9.exe", "tomcat9w.exe"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	prunsrv, monitor := resolvePrunsrvSource(dir)
	if !strings.HasSuffix(prunsrv, "tomcat10.exe") {
		t.Fatalf("expected tomcat10.exe, got %s", prunsrv)
	}
	if !strings.HasSuffix(monitor, "tomcat10w.exe") {
		t.Fatalf("expected tomcat10w.exe, got %s", monitor)
	}
}

func TestResolvePrunsrvSource_Neither(t *testing.T) {
	dir := t.TempDir()
	prunsrv, monitor := resolvePrunsrvSource(dir)
	if prunsrv != "" || monitor != "" {
		t.Fatalf("expected empty, got prunsrv=%q monitor=%q", prunsrv, monitor)
	}
}

func TestRegenService_CopiesPrunsrvAndMonitor(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only behaviour")
	}
	cfg := mustCfg(t)
	bin := filepath.Join(cfg.TomcatHome, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Plant tomcat10.exe / tomcat10w.exe and a server.xml with port 9090
	// so resolveServiceName picks up "Tomcat9090".
	conf := filepath.Join(cfg.TomcatHome, "conf")
	if err := os.MkdirAll(conf, 0o755); err != nil {
		t.Fatalf("mkdir conf: %v", err)
	}
	xml := `<?xml version="1.0"?>
<Server port="8005" shutdown="SHUTDOWN"><Service name="Catalina"><Connector port="9090"/></Service></Server>
`
	if err := os.WriteFile(filepath.Join(conf, "server.xml"), []byte(xml), 0o644); err != nil {
		t.Fatalf("write server.xml: %v", err)
	}
	srcPrunsrv := []byte("prunsrv-bytes")
	srcMonitor := []byte("monitor-bytes")
	if err := os.WriteFile(filepath.Join(bin, "tomcat10.exe"), srcPrunsrv, 0o644); err != nil {
		t.Fatalf("write prunsrv src: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bin, "tomcat10w.exe"), srcMonitor, 0o644); err != nil {
		t.Fatalf("write monitor src: %v", err)
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
	if got.ServiceName != "Tomcat9090" {
		t.Fatalf("service name = %q, want Tomcat9090", got.ServiceName)
	}
	if !got.PrunsrvCopied || !got.MonitorCopied {
		t.Fatalf("expected both copies, got prunsrv=%v monitor=%v", got.PrunsrvCopied, got.MonitorCopied)
	}
	for _, p := range []string{got.PrunsrvPath, got.MonitorPath} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected file at %s: %v", p, err)
		}
	}
	// Second call should report already-existed without overwriting.
	raw, err = reg.regenService(nil)
	if err != nil {
		t.Fatalf("regenService second call: %v", err)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal 2: %v", err)
	}
	if !got.PrunsrvExisted || !got.MonitorExisted {
		t.Fatalf("expected already-existed on second call, got prunsrv=%v monitor=%v", got.PrunsrvExisted, got.MonitorExisted)
	}
	if got.PrunsrvCopied || got.MonitorCopied {
		t.Fatalf("did not expect copies on second call")
	}
}

func TestRegenService_NoPrunsrvSkipsCopies(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only behaviour")
	}
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
	if got.PrunsrvCopied || got.MonitorCopied || got.PrunsrvExisted || got.MonitorExisted {
		t.Fatalf("expected no copy activity, got %+v", got)
	}
	if got.PrunsrvSource != "" || got.MonitorSource != "" {
		t.Fatalf("expected empty source paths, got prunsrv=%q monitor=%q", got.PrunsrvSource, got.MonitorSource)
	}
}

func TestBuildWindowsService_ContainsRequiredAnchors(t *testing.T) {
	cfg := mustCfg(t)
	cfg.LogDir = filepath.Join(cfg.TomcatHome, "logs")
	cfg.ServiceName = "Tomcat8080"
	out := buildWindowsService(cfg, "Tomcat8080")
	anchors := []string{
		`set "JAVA_HOME=`,
		`set "CATALINA_HOME=`,
		`if /I "%1"=="start"`,
		`if /I "%1"=="stop"`,
		`if /I "%1"=="restart"`,
		`if /I "%1"=="install"`,
		`if /I "%1"=="remove"`,
		`net start Tomcat8080`,
		`net stop Tomcat8080`,
		`--Install="`,
		`--JavaHome="`,
		`--CatalinaHome="`,
		`sc query Tomcat8080`,
	}
	for _, a := range anchors {
		if !strings.Contains(out, a) {
			t.Errorf("missing anchor %q in service.bat output:\n%s", a, out)
		}
	}
	// Must NOT escape the brace characters in the usage line any more —
	// the previous version rendered `usage: service.bat {install^|remove^|...`
	// which is correct; the previous-sh version had `{{start|stop|restart}}`
	// which was the Go-template placeholder bug. The Windows side never
	// had that bug, but pin it explicitly so a future refactor doesn't
	// reintroduce template placeholders.
	if strings.Contains(out, "{{") || strings.Contains(out, "}}") {
		t.Fatalf("service.bat output contains Go template placeholder fragments:\n%s", out)
	}
}

func TestBuildUnixService_NoTemplatePlaceholders(t *testing.T) {
	cfg := mustCfg(t)
	out := buildUnixService(cfg)
	if strings.Contains(out, "{{") || strings.Contains(out, "}}") {
		t.Fatalf("service.sh output still contains {{...}} Go template placeholders:\n%s", out)
	}
	if !strings.Contains(out, "usage: $0 {start|stop|restart}") {
		t.Fatalf("expected clean usage line in service.sh, got:\n%s", out)
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
		{"text": "x"}, // missing path
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

// TestValidateConfig_NoConnectors covers the "esb/erp blocks absent from
// config.json" case: connectors map is empty, tomcat_home / jdk_home /
// log_dir each report their own existence, allowed_dirs entries each
// get an existence flag. Passwords must never appear.
func TestValidateConfig_NoConnectors(t *testing.T) {
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t))
	raw, err := reg.validateConfig(context.Background())
	if err != nil {
		t.Fatalf("validateConfig: %v", err)
	}
	var got ValidateData
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.TomcatHome != cfg.TomcatHome {
		t.Fatalf("TomcatHome = %q, want %q", got.TomcatHome, cfg.TomcatHome)
	}
	if !got.TomcatExists || !got.JDKExists || !got.LogDirExists {
		t.Fatalf("existence flags wrong: tomcat=%v jdk=%v log=%v",
			got.TomcatExists, got.JDKExists, got.LogDirExists)
	}
	if len(got.AllowedDirs) != len(cfg.AllowedDirs) {
		t.Fatalf("AllowedDirs len = %d, want %d", len(got.AllowedDirs), len(cfg.AllowedDirs))
	}
	if got.Connectors == nil {
		t.Fatal("Connectors map is nil; want empty (but present)")
	}
	if len(got.Connectors) != 0 {
		t.Fatalf("Connectors should be empty when none wired, got %v", got.Connectors)
	}
	// No password field exists by construction — nothing to assert on the
	// raw bytes here, but verify the JSON has no "password" key anywhere.
	if strings.Contains(string(raw), "password") {
		t.Fatalf("validate_config response leaked a password field: %s", raw)
	}
}

// TestValidateConfig_MissingDirs covers the case where one of the
// configured dirs is gone (typo'd in config.json after editing) — the
// report must still come back, just with exists=false. This is the
// failure mode the operator would otherwise have to discover by
// tail-ing helper.log.
func TestValidateConfig_MissingDirs(t *testing.T) {
	cfg := mustCfg(t)
	cfg.TomcatHome = filepath.Join(t.TempDir(), "does-not-exist")
	cfg.JDKHome = filepath.Join(t.TempDir(), "also-missing")
	cfg.LogDir = filepath.Join(t.TempDir(), "logs-missing")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	reg := NewRegistry(cfg, mustExec(t))
	raw, err := reg.validateConfig(context.Background())
	if err != nil {
		t.Fatalf("validateConfig: %v", err)
	}
	var got ValidateData
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.TomcatExists || got.JDKExists || got.LogDirExists {
		t.Fatalf("existence flags should be false for missing dirs, got %+v", got)
	}
}

// TestValidateConfig_WithConnectors ensures each registered connector
// surfaces its Status snapshot, including the LastError string when
// the FakeOp reports Open=false. This is the post-edit-of-config.json
// verification path: operator restarts helper, calls validate_config,
// and immediately sees whether the new password took.
func TestValidateConfig_WithConnectors(t *testing.T) {
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t)).
		WithDB("esb_db_operator", &dbop.FakeOp{
			Driver: "mysql", Host: "h", Database: "d",
			Open: true,
		}).
		WithDB("erp_db_operator", &dbop.FakeOp{
			Driver: "oracle", Host: "oh", Database: "ORCL",
			CurrentSchema: "BFDHERP",
			Open:          false, LastError: "ORA-01017: invalid username/password",
		})
	raw, err := reg.validateConfig(context.Background())
	if err != nil {
		t.Fatalf("validateConfig: %v", err)
	}
	var got ValidateData
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Connectors) != 2 {
		t.Fatalf("Connectors = %d, want 2", len(got.Connectors))
	}
	esb, ok := got.Connectors["esb_db_operator"]
	if !ok {
		t.Fatal("missing esb_db_operator in result")
	}
	if !esb.Open || esb.Driver != "mysql" || esb.Host != "h" || esb.Database != "d" {
		t.Fatalf("esb snapshot wrong: %+v", esb)
	}
	erp, ok := got.Connectors["erp_db_operator"]
	if !ok {
		t.Fatal("missing erp_db_operator in result")
	}
	if erp.Open {
		t.Fatal("erp Open should be false (FakeOp)")
	}
	if erp.CurrentSchema != "BFDHERP" {
		t.Fatalf("erp CurrentSchema = %q, want BFDHERP", erp.CurrentSchema)
	}
	if !strings.Contains(erp.LastError, "ORA-01017") {
		t.Fatalf("erp LastError should carry the ORA-01017 message, got %q", erp.LastError)
	}
}

// TestValidateConfig_ReportsConfigLoadedAt covers the "is my helper running
// the new config.json?" question. The operator compares this timestamp
// against config.json's mtime on disk; a stale value means helper has not
// been restarted since the last edit (problem.md feedback ⑤).
func TestValidateConfig_ReportsConfigLoadedAt(t *testing.T) {
	cfg := mustCfg(t)
	when := time.Now().Add(-3 * time.Minute)
	cfg.LoadedAt = when
	reg := NewRegistry(cfg, mustExec(t))
	raw, err := reg.validateConfig(context.Background())
	if err != nil {
		t.Fatalf("validateConfig: %v", err)
	}
	var got ValidateData
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ConfigLoadedAt.IsZero() {
		t.Fatal("ConfigLoadedAt missing from validateConfig response")
	}
	if d := time.Since(got.ConfigLoadedAt); d < 2*time.Minute || d > 4*time.Minute {
		t.Fatalf("ConfigLoadedAt not preserved (delta=%s, want ~3m)", d)
	}
}

// TestPowerShellWrap covers the helper that translates a plain command +
// work_dir into a single `powershell -EncodedCommand <base64>` string.
// The output is what gets handed to Executor.Exec. Encoding is the fix
// for the 2026-09-15 G3 report: the old plain `-Command "…"` wrapper was
// echoed back instead of executed because cmd.exe ate the quoting.
func TestPowerShellWrap(t *testing.T) {
	const prefix = "powershell -NoProfile -NonInteractive -EncodedCommand "
	cases := []struct {
		name string
		cmd  string
		work string
		must []string // substrings that must appear in the decoded script
	}{
		{
			name: "plain",
			cmd:  "Get-Process mysqld",
			work: `D:\bs-esb`,
			must: []string{
				`Set-Location -LiteralPath 'D:\bs-esb'; Get-Process mysqld`,
			},
		},
		{
			name: "path with spaces stays untouched in workDir argument",
			cmd:  "Get-ChildItem",
			work: `D:\Program Files\MySQL`,
			must: []string{
				`Set-Location -LiteralPath 'D:\Program Files\MySQL'; Get-ChildItem`,
			},
		},
		{
			name: "single quotes pass through verbatim",
			cmd:  `Write-Output 'hello'`,
			work: `D:\bs-esb`,
			must: []string{
				`Write-Output 'hello'`,
			},
		},
		{
			name: "double quotes and semicolons survive verbatim",
			cmd:  `Write-Output "hello world"; Get-Process mysqld`,
			work: `D:\bs-esb`,
			must: []string{
				`Write-Output "hello world"; Get-Process mysqld`,
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := powershellWrap(c.cmd, c.work)
			if !strings.HasPrefix(got, prefix) {
				t.Fatalf("wrapper must be an -EncodedCommand line, got: %s", got)
			}
			// The whole point: no quotes / metacharacters for cmd.exe
			// to mangle, and no raw newlines.
			if strings.ContainsAny(got, `"&|<>;`+"\r\n") {
				t.Fatalf("wrapped command must not contain cmd.exe metacharacters: %s", got)
			}
			raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(got, prefix))
			if err != nil {
				t.Fatalf("payload is not valid base64: %v", err)
			}
			script := utf16Decode(raw)
			for _, m := range c.must {
				if !strings.Contains(script, m) {
					t.Errorf("missing %q in decoded script: %s", m, script)
				}
			}
		})
	}
}

// utf16Decode is the inverse of actions.utf16LEBytes, test-only.
func utf16Decode(b []byte) string {
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return string(utf16.Decode(units))
}

// TestExecAction_DefaultWorkDirFallsBack covers G1②: with a stale
// tomcat_home (missing / not in allowed_dirs), exec without an explicit
// work_dir must still run, in the first existing allowed_dirs entry, and
// say so in the result.
func TestExecAction_DefaultWorkDirFallsBack(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.HelperConfig{
		ServerURL:         "wss://x",
		Token:             "longenough",
		TomcatHome:        filepath.Join(dir, "no-such-tomcat"),
		JDKHome:           dir,
		LogDir:            dir,
		AllowedDirs:       []string{dir},
		CommandTimeout:    30 * time.Second,
		MaxOutputSize:     4096,
		ReconnectInterval: time.Second,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	reg := NewRegistry(cfg, execshell.NewExecutor(cfg).WithRunner(execshell.NewOSRunner()))

	raw, err := reg.execAction(context.Background(), mustMarshal(map[string]any{"cmd": "echo hi"}))
	if err != nil {
		t.Fatalf("execAction: %v", err)
	}
	var got struct {
		Stdout   string `json:"stdout"`
		WorkDir  string `json:"work_dir"`
		Fallback bool   `json:"work_dir_fallback"`
		Default  string `json:"work_dir_default"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Fallback {
		t.Fatalf("expected work_dir_fallback=true, got %s", raw)
	}
	if got.WorkDir != dir {
		t.Fatalf("work_dir = %q, want %q", got.WorkDir, dir)
	}
	if got.Default != cfg.TomcatHome {
		t.Fatalf("work_dir_default = %q, want %q", got.Default, cfg.TomcatHome)
	}
	if !strings.Contains(got.Stdout, "hi") {
		t.Fatalf("stdout missing 'hi': %q", got.Stdout)
	}
}

// TestExecAction_PowerShellShellExecutes is the end-to-end G3 check:
// exec with shell=powershell must actually run the command (not echo it)
// and land in work_dir.
func TestExecAction_PowerShellShellExecutes(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("powershell wrapper is Windows-targeted")
	}
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, execshell.NewExecutor(cfg).WithRunner(execshell.NewOSRunner()))
	raw, err := reg.execAction(context.Background(), mustMarshal(map[string]any{
		"cmd":   `Write-Output "hello world"; (Get-Location).Path`,
		"shell": "powershell",
	}))
	if err != nil {
		t.Fatalf("execAction: %v", err)
	}
	var got struct {
		Stdout   string `json:"stdout"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ExitCode != 0 || !strings.Contains(got.Stdout, "hello world") {
		t.Fatalf("powershell did not execute: exit=%d stdout=%q", got.ExitCode, got.Stdout)
	}
	if !strings.Contains(strings.ToLower(got.Stdout), strings.ToLower(cfg.TomcatHome)) {
		t.Fatalf("Set-Location prologue missing? stdout=%q want %q", got.Stdout, cfg.TomcatHome)
	}
}

// TestExecAction_ExplicitWorkDirStillRejected makes sure the fallback
// only applies to the implicit default: an explicit work_dir outside
// allowed_dirs keeps failing.
func TestExecAction_ExplicitWorkDirStillRejected(t *testing.T) {
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t))
	_, err := reg.execAction(context.Background(), mustMarshal(map[string]any{
		"cmd":      "echo hi",
		"work_dir": filepath.FromSlash("/etc"),
	}))
	if err == nil {
		t.Fatal("expected error for explicit work_dir outside allowed_dirs")
	}
	if !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("error should mention 'not allowed', got %v", err)
	}
}

// TestExecAction_RejectsUnknownShell documents the validation: the
// caller passes a typo'd shell name and we return a clear error
// instead of silently using cmd /C.
func TestExecAction_RejectsUnknownShell(t *testing.T) {
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t))
	_, err := reg.execAction(context.Background(), mustMarshal(map[string]any{
		"cmd":   "echo hi",
		"shell": "fish",
	}))
	if err == nil {
		t.Fatal("expected error for unknown shell")
	}
	if !strings.Contains(err.Error(), "unknown shell") {
		t.Fatalf("error message should mention 'unknown shell', got %v", err)
	}
}

// TestTools_SchemasAreValidJSON catches a class of bugs we hit on
// 2026-09-14: a description string in execSchema contained an
// unescaped inner `"`, which made the whole ToolList body
// non-JSON. Helper.PostConnect then failed to send it, the helper
// looped reconnecting, and the customer box couldn't list tools.
// Every inputSchema shipped to clients must round-trip through
// json.Unmarshal — anything else is a bug.
func TestTools_SchemasAreValidJSON(t *testing.T) {
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t)).
		WithDB("esb_db_operator", &dbop.FakeOp{Driver: "mysql"}).
		WithDB("erp_db_operator", &dbop.FakeOp{Driver: "oracle"})
	for _, t0 := range reg.Tools() {
		t.Run(t0.Name, func(t *testing.T) {
			var v any
			if err := json.Unmarshal(t0.InputSchema, &v); err != nil {
				t.Fatalf("inputSchema for %s is not valid JSON: %v\nschema: %s", t0.Name, err, string(t0.InputSchema))
			}
			// And it must be an object (MCP / tool-use convention).
			if _, ok := v.(map[string]any); !ok {
				t.Fatalf("inputSchema for %s must be a JSON object, got %T", t0.Name, v)
			}
		})
	}
}

// TestTools_HaveNewActions ensures stop_service, start_service,
// wait_for_catalina, deploy, and the updated restart schema are all
// present in the tool list. This is a smoke test for the
// problem.md #1/#2/#4 fixes: the new atomic actions must exist.
func TestTools_HaveNewActions(t *testing.T) {
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t))
	names := map[string]bool{}
	for _, tool := range reg.Tools() {
		names[tool.Name] = true
	}
	for _, want := range []string{"stop_service", "start_service", "wait_for_catalina", "deploy"} {
		if !names[want] {
			t.Fatalf("missing tool %q", want)
		}
	}
	if !names["restart"] {
		t.Fatal("missing restart tool")
	}
}

// TestTools_RestartSchemaHasSeparateTimeouts ensures the restart schema
// includes stop_timeout and start_timeout (problem.md #1 fix).
func TestTools_RestartSchemaHasSeparateTimeouts(t *testing.T) {
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t))
	var restartSchema json.RawMessage
	for _, tool := range reg.Tools() {
		if tool.Name == "restart" {
			restartSchema = tool.InputSchema
			break
		}
	}
	if restartSchema == nil {
		t.Fatal("restart tool not found")
	}
	var s map[string]any
	if err := json.Unmarshal(restartSchema, &s); err != nil {
		t.Fatalf("restart schema not valid JSON: %v", err)
	}
	props, ok := s["properties"].(map[string]any)
	if !ok {
		t.Fatal("restart schema has no properties")
	}
	if _, ok := props["stop_timeout"]; !ok {
		t.Fatal("restart schema missing stop_timeout")
	}
	if _, ok := props["start_timeout"]; !ok {
		t.Fatal("restart schema missing start_timeout")
	}
}

// TestKillZombie_MatchesByPIDRegardlessOfInteresting ensures that
// kill_zombie pid=X matches the PID even when isInteresting does not
// catch the process (problem.md #3 fix).
func TestKillZombie_MatchesByPIDRegardlessOfInteresting(t *testing.T) {
	cfg := mustCfg(t)
	probe := &fakeProbe{procs: []JavaProcess{
		{PID: 99991, Cmdline: "java -jar random-app.jar"}, // not interesting
		{PID: 99992, Cmdline: "java -Dcatalina.home=/opt/tomcat org.apache.catalina.startup.Bootstrap"},
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
	if len(got.Found) != 1 || got.Found[0] != 99991 {
		t.Fatalf("expected pid 99991 found (regardless of isInteresting), got %+v", got.Found)
	}
}

// TestStopService_UnknownServiceName returns an error when the
// service name cannot be determined (no cfg.ServiceName and no
// sc query results).
func TestStopService_UnknownServiceName(t *testing.T) {
	cfg := mustCfg(t)
	cfg.ServiceName = "" // force discover which needs sc
	reg := NewRegistry(cfg, mustExec(t))
	_, err := reg.stopService(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for unknown service name")
	}
}

// TestStartService_UnknownServiceName returns an error when the
// service name cannot be determined.
func TestStartService_UnknownServiceName(t *testing.T) {
	cfg := mustCfg(t)
	cfg.ServiceName = ""
	reg := NewRegistry(cfg, mustExec(t))
	_, err := reg.startService(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for unknown service name")
	}
}

// TestWaitCatalina_GoneReturnsEarly ensures wait_for_catalina with
// state=gone returns immediately when no catalina processes exist.
func TestWaitCatalina_GoneReturnsEarly(t *testing.T) {
	cfg := mustCfg(t)
	probe := &fakeProbe{procs: []JavaProcess{}}
	reg := NewRegistry(cfg, mustExec(t)).WithProbe(probe)
	raw, err := reg.waitCatalina(context.Background(), mustMarshal(map[string]any{"state": "gone"}))
	if err != nil {
		t.Fatalf("waitCatalina: %v", err)
	}
	var got WaitCatalinaData
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Exited {
		t.Fatal("expected Exited=true when no catalina processes")
	}
	if got.Wanted != "gone" {
		t.Fatalf("expected Wanted='gone', got %q", got.Wanted)
	}
}

// TestDeploy_ReturnsPartialResultOnStopFailure ensures deploy returns
// a partial result (with step set) when stop_service fails, rather
// than a hard error — so the caller can see what happened.
func TestDeploy_StopFailureReturnsPartialResult(t *testing.T) {
	cfg := mustCfg(t)
	cfg.ServiceName = "" // force unknown service name → stop_service fails
	reg := NewRegistry(cfg, mustExec(t))
	raw, err := reg.deploy(context.Background(), mustMarshal(map[string]any{
		"src": "a.jar", "dest": "b.jar",
	}))
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	var got DeployData
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Step != "stop_service" {
		t.Fatalf("expected step='stop_service', got %q", got.Step)
	}
	if got.Error == "" {
		t.Fatal("expected Error to be set on stop failure")
	}
}

// TestReplaceJar_WithRestartFalse_ReturnsDataWithoutRestartFields
// ensures replace_jar with restart=false returns data without
// restart_data or restart_error fields.
func TestReplaceJar_WithRestartFalse_ReturnsDataWithoutRestartFields(t *testing.T) {
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
	if got.Restarted {
		t.Fatal("restart=false should not set Restarted")
	}
	if got.RestartData != nil {
		t.Fatal("restart=false should not set RestartData")
	}
}

// TestStatus_ReportsProbeErrors ensures status degrades gracefully when a
// probe fails: the section stays empty but the reason is surfaced in
// java_probe_error / ports_probe_error instead of failing the whole call
// (2026-09-25 report: silent nulls are undebuggable).
func TestStatus_ReportsProbeErrors(t *testing.T) {
	cfg := mustCfg(t)
	probe := &fakeProbe{
		javaErr:  errors.New("wmic: not found"),
		portsErr: errors.New("netstat: boom"),
	}
	reg := NewRegistry(cfg, mustExec(t)).WithProbe(probe)
	raw, err := reg.status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var got StatusData
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.JavaProbeError == "" || !strings.Contains(got.JavaProbeError, "wmic") {
		t.Fatalf("expected java_probe_error with reason, got %q", got.JavaProbeError)
	}
	if got.PortsProbeError == "" || !strings.Contains(got.PortsProbeError, "netstat") {
		t.Fatalf("expected ports_probe_error with reason, got %q", got.PortsProbeError)
	}
	if got.JavaProcs != nil || got.Ports != nil {
		t.Fatalf("expected nil probe sections, got %+v / %+v", got.JavaProcs, got.Ports)
	}
	if !got.TomcatExists {
		t.Fatal("tomcat_exists should still be reported")
	}
}

// TestJavaProcessesWindows_ParserIncludesPrunsrvServiceProcess covers the
// 2026-09-25 report: Tomcat installed as a Windows service with procrun
// StartMode=jvm runs the JVM inside the renamed service binary
// (tomcat10-esb.exe), so enumeration must not filter on java.exe alone.
func TestJavaProcessesWindows_ParserIncludesPrunsrvServiceProcess(t *testing.T) {
	wmicOut := "CommandLine=D:\\bs-esb\\apache-tomcat-10.1.59\\bin\\tomcat10-esb.exe //RS//tomcat10-esb\r\n" +
		"Name=tomcat10-esb.exe\r\n" +
		"ProcessId=4242\r\n" +
		"\r\n" +
		"CommandLine=C:\\jdk\\bin\\java -Dcatalina.home=D:\\tomcat org.apache.catalina.startup.Bootstrap\r\n" +
		"Name=java.exe\r\n" +
		"ProcessId=1111\r\n" +
		"\r\n" +
		"CommandLine=C:\\Windows\\explorer.exe\r\n" +
		"Name=explorer.exe\r\n" +
		"ProcessId=2222\r\n" +
		"\r\n"
	procs := filterInterestingProcesses(parseWmicList(wmicOut))
	if len(procs) != 2 {
		t.Fatalf("expected 2 kept processes (prunsrv + java), got %d: %+v", len(procs), procs)
	}
	pids := map[int]bool{}
	for _, p := range procs {
		pids[p.PID] = true
	}
	if !pids[4242] || !pids[1111] || pids[2222] {
		t.Fatalf("wrong process set kept: %+v", procs)
	}

	csvOut := "\"ProcessId\",\"Name\",\"CommandLine\"\n" +
		"\"4242\",\"tomcat10-esb.exe\",\"D:\\bs-esb\\bin\\tomcat10-esb.exe //RS//tomcat10-esb\"\n" +
		"\"2222\",\"explorer.exe\",\"C:\\Windows\\explorer.exe\"\n"
	csvProcs := filterInterestingProcesses(parsePowerShellCSV(csvOut))
	if len(csvProcs) != 1 || csvProcs[0].PID != 4242 || csvProcs[0].Name != "tomcat10-esb.exe" {
		t.Fatalf("CSV parse kept wrong set: %+v", csvProcs)
	}
}

// TestIsInteresting_NameOnlyPrunsrvProcess ensures a prunsrv-hosted JVM with
// an empty CommandLine is still matched via its image name.
func TestIsInteresting_NameOnlyPrunsrvProcess(t *testing.T) {
	p := JavaProcess{PID: 99, Name: "Tomcat10-ESB.exe"}
	if !isInteresting(p, "D:\\tomcat") {
		t.Fatal("prunsrv image name should be interesting")
	}
	if isInteresting(JavaProcess{PID: 1, Name: "explorer.exe"}, "") {
		t.Fatal("explorer should not be interesting")
	}
}

// TestDeploy_StartFailureAttemptsFallbackAndRecovers covers the 2026-09-25
// report: when the start step fails, deploy must make one state-aware
// fallback attempt so the service does not stay silently stopped. First
// sc start fails; the fallback sees STOPPED, restarts, and reaches RUNNING.
func TestDeploy_StartFailureAttemptsFallbackAndRecovers(t *testing.T) {
	cfg := mustCfg(t)
	cfg.ServiceName = "Tomcat10ESB"
	seedDeployJars(t, cfg)

	runner := &scriptRunner{rules: []scriptRule{
		{match: `sc stop "Tomcat10ESB"`},
		// sc query hits: (1) fallback state check → STOPPED,
		// (2) waitServiceState in the retry → RUNNING, (3+) final state.
		{match: `sc query "Tomcat10ESB"`, stdouts: []string{
			"SERVICE_NAME: Tomcat10ESB\n        STATE              : 1  STOPPED\n",
			"SERVICE_NAME: Tomcat10ESB\n        STATE              : 4  RUNNING\n",
		}},
		{match: `sc start "Tomcat10ESB"`, failFirst: 1, stderr: "The service did not respond."},
	}}
	exec := execshell.NewExecutor(cfg).WithRunner(runner)
	reg := NewRegistry(cfg, exec).WithProbe(&fakeProbe{})

	raw, err := reg.deploy(context.Background(), mustMarshal(map[string]any{
		"src":  filepath.Join(cfg.TomcatHome, "uploads", "app.jar"),
		"dest": filepath.Join(cfg.TomcatHome, "webapps", "app.jar"),
	}))
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	var got DeployData
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Step != "start_service" {
		t.Fatalf("expected step=start_service, got %q", got.Step)
	}
	if got.Error == "" {
		t.Fatal("original start error must be reported")
	}
	if got.StartFallback == nil || !got.StartFallback.Running {
		t.Fatalf("expected recovered fallback (Running=true), got %+v (fallback_error=%q)", got.StartFallback, got.FallbackError)
	}
	if got.FallbackError != "" {
		t.Fatalf("unexpected fallback_error: %q", got.FallbackError)
	}
	if got.FinalState != "RUNNING" {
		t.Fatalf("expected final_state=RUNNING, got %q", got.FinalState)
	}
	// Replace must still have happened.
	if got.ReplaceResult == nil || got.ReplaceResult.Bytes == 0 {
		t.Fatalf("expected replace_result with bytes, got %+v", got.ReplaceResult)
	}
}

// TestDeploy_FallbackAlsoFailsReportsIt ensures that when the fallback start
// also fails, deploy reports why instead of pretending everything is fine.
func TestDeploy_FallbackAlsoFailsReportsIt(t *testing.T) {
	cfg := mustCfg(t)
	cfg.ServiceName = "Tomcat10ESB"
	seedDeployJars(t, cfg)

	runner := &scriptRunner{rules: []scriptRule{
		{match: `sc stop "Tomcat10ESB"`},
		{match: `sc query "Tomcat10ESB"`, stdouts: []string{
			"SERVICE_NAME: Tomcat10ESB\n        STATE              : 1  STOPPED\n",
		}},
		{match: `sc start "Tomcat10ESB"`, stderr: "start refused", failFirst: 1000},
	}}
	exec := execshell.NewExecutor(cfg).WithRunner(runner)
	reg := NewRegistry(cfg, exec).WithProbe(&fakeProbe{})

	raw, err := reg.deploy(context.Background(), mustMarshal(map[string]any{
		"src":  filepath.Join(cfg.TomcatHome, "uploads", "app.jar"),
		"dest": filepath.Join(cfg.TomcatHome, "webapps", "app.jar"),
	}))
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	var got DeployData
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Step != "start_service" || got.Error == "" {
		t.Fatalf("expected start failure recorded, got step=%q error=%q", got.Step, got.Error)
	}
	if got.StartFallback != nil {
		t.Fatalf("expected no fallback data on hard failure, got %+v", got.StartFallback)
	}
	if got.FallbackError == "" {
		t.Fatal("fallback failure must be reported in fallback_error")
	}
	if got.FinalState != "STOPPED" {
		t.Fatalf("expected final_state=STOPPED, got %q", got.FinalState)
	}
}

// seedDeployJars creates the src/dest jar pair deploy tests copy around.
func seedDeployJars(t *testing.T, cfg *config.HelperConfig) {
	t.Helper()
	src := filepath.Join(cfg.TomcatHome, "uploads", "app.jar")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatalf("mkdir uploads: %v", err)
	}
	if err := os.WriteFile(src, []byte("NEW-JAR"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	dst := filepath.Join(cfg.TomcatHome, "webapps", "app.jar")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("mkdir webapps: %v", err)
	}
	if err := os.WriteFile(dst, []byte("OLD-JAR"), 0o644); err != nil {
		t.Fatalf("write dst: %v", err)
	}
}
