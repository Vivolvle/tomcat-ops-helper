package helpergui

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestProcManagerLifecycle drives the real spawn/reap machinery using
// cmd.exe as a stand-in child: ProcManager spawns `<helperPath> -config
// <cfg>`, and cmd.exe treats -config as an invalid switch, prints a
// usage error to the piped stdout, and exits non-zero almost
// immediately. That exercises: stdout → ring, the Wait reaper recording
// the exit code, and Status transitioning back to SourceNone.
func TestProcManagerLifecycle(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("lifecycle test is windows-only (taskkill / tasklist)")
	}
	cmdPath, err := exec.LookPath("cmd.exe")
	if err != nil {
		t.Skip(err)
	}
	pm := NewProcManager(cmdPath, filepath.Join(t.TempDir(), "config.json"))
	if _, err := pm.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if st := pm.Status(); !st.Running || st.Source != SourceGUI {
		t.Fatalf("status right after start: %+v", st)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if st := pm.Status(); !st.Running {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	st := pm.Status()
	if st.Running {
		t.Fatalf("cmd.exe with a bogus switch must exit fast: %+v", st)
	}
	if st.ExitCode == nil {
		t.Fatal("reaper must record the exit code")
	}
	lines, next, _, _ := pm.Ring().Since(0, 100)
	if next == 0 {
		t.Fatal("ring must contain the lifecycle + stdout lines")
	}
	sawExitMarker := false
	for _, l := range lines {
		if l.Stream == "system" && strings.HasPrefix(l.Text, "helper 退出") {
			sawExitMarker = true
		}
	}
	if !sawExitMarker {
		t.Fatalf("ring must contain the helper 退出 lifecycle marker, got %+v", lines)
	}
}

// TestProcManagerStopWithoutChild: stopping with nothing running is an
// error, not a crash.
func TestProcManagerStopWithoutChild(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only")
	}
	pm := NewProcManager("cmd.exe", filepath.Join(t.TempDir(), "config.json"))
	if _, err := pm.Stop(time.Second); err == nil {
		t.Fatal("expected error when no child is running")
	}
}
