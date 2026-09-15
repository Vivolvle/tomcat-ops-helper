package actions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestRegenService_BatIsParseable exercises the real output of buildWindowsService
// end-to-end: writes the bat via regenService, scans it for the structural
// anchors an operator actually needs (JAVA_HOME set before any prunsrv call,
// install/remove/start/stop/restart branches, correct prunsrv path).
func TestRegenService_BatIsParseable(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only behaviour")
	}
	cfg := mustCfg(t)
	bin := filepath.Join(cfg.TomcatHome, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	conf := filepath.Join(cfg.TomcatHome, "conf")
	if err := os.MkdirAll(conf, 0o755); err != nil {
		t.Fatalf("mkdir conf: %v", err)
	}
	xml := `<?xml version="1.0"?>
<Server port="8005" shutdown="SHUTDOWN"><Service name="Catalina"><Connector port="9090" protocol="HTTP/1.1"/></Service></Server>
`
	if err := os.WriteFile(filepath.Join(conf, "server.xml"), []byte(xml), 0o644); err != nil {
		t.Fatalf("write server.xml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bin, "tomcat10.exe"), []byte("prunsrv"), 0o644); err != nil {
		t.Fatalf("write tomcat10.exe: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bin, "tomcat10w.exe"), []byte("monitor"), 0o644); err != nil {
		t.Fatalf("write tomcat10w.exe: %v", err)
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
	body, err := os.ReadFile(got.Path)
	if err != nil {
		t.Fatalf("read service.bat: %v", err)
	}
	text := string(body)

	// service name must come from server.xml (port 9090 → Tomcat9090).
	if got.ServiceName != "Tomcat9090" {
		t.Fatalf("ServiceName = %q, want Tomcat9090", got.ServiceName)
	}
	// Copy happened, both prunsrv and monitor.
	if !got.PrunsrvCopied || !got.MonitorCopied {
		t.Fatalf("expected both copies, got prunsrv=%v monitor=%v", got.PrunsrvCopied, got.MonitorCopied)
	}
	// Bat mentions Tomcat9090 + the binary paths.
	wantPath := filepath.Join(bin, "Tomcat9090.exe")
	if !strings.Contains(text, wantPath) {
		t.Errorf("service.bat does not mention prunsrv path %s:\n%s", wantPath, text)
	}
	// JAVA_HOME / CATALINA_HOME come before any prunsrv call.
	idxJava := strings.Index(text, `set "JAVA_HOME=`)
	idxCatalina := strings.Index(text, `set "CATALINA_HOME=`)
	idxInstall := strings.Index(text, `if /I "%1"=="install"`)
	if !(idxJava >= 0 && idxJava < idxInstall) {
		t.Errorf("JAVA_HOME not set before install branch: javaIdx=%d installIdx=%d", idxJava, idxInstall)
	}
	if !(idxCatalina >= 0 && idxCatalina < idxInstall) {
		t.Errorf("CATALINA_HOME not set before install branch: catalinaIdx=%d installIdx=%d", idxCatalina, idxInstall)
	}
	// All five subcommands wired up.
	for _, sub := range []string{"install", "remove", "start", "stop", "restart"} {
		if !strings.Contains(text, `if /I "%1"=="`+sub+`"`) {
			t.Errorf("missing %q branch", sub)
		}
	}
	// The usage line should not carry template placeholders.
	if strings.Contains(text, "{{") || strings.Contains(text, "}}") {
		t.Errorf("service.bat still has Go template placeholders:\n%s", text)
	}
}
