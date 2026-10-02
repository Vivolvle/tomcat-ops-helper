package helpergui

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ProcSource explains where a helper.exe comes from.
type ProcSource string

const (
	SourceNone     ProcSource = "none"     // not running
	SourceGUI      ProcSource = "gui"      // child of this GUI session
	SourceExternal ProcSource = "external" // started outside the GUI (service / run-helper.bat / orphan)
)

// ProcState is the reply of GET /api/proc/status.
type ProcState struct {
	Running  bool       `json:"running"`
	Source   ProcSource `json:"source"`
	PID      int        `json:"pid,omitempty"`
	ExitCode *int       `json:"exit_code,omitempty"`
	Err      string     `json:"error,omitempty"`
}

// ProcManager owns the helper.exe child process: spawn with piped
// stdio feeding the log ring, tree-kill on stop (helper legitimately
// has a process tree — its exec tool spawns cmd children), and
// external-instance detection for helpers not started by us.
type ProcManager struct {
	helperPath string
	cfgPath    string

	mu       sync.Mutex
	cmd      *exec.Cmd
	exitCode *int
	ring     *LogRing
}

// NewProcManager wires a manager for helperPath launched with cfgPath.
func NewProcManager(helperPath, cfgPath string) *ProcManager {
	return &ProcManager{
		helperPath: helperPath,
		cfgPath:    cfgPath,
		ring:       NewLogRing(5000),
	}
}

// Ring exposes the shared log ring (used by the /api/proc/logs route).
func (pm *ProcManager) Ring() *LogRing { return pm.ring }

// Start spawns helper.exe with the configured config, stdout+stderr
// piped into the ring. Working directory matches run-helper.bat (the
// config's directory) so relative log_file paths resolve identically.
func (pm *ProcManager) Start() (int, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if pm.cmd != nil && pm.cmd.Process != nil {
		// Already running under us: is the process actually alive?
		if pm.cmd.ProcessState == nil {
			return pm.cmd.Process.Pid, fmt.Errorf("helper 已在运行 (PID %d)", pm.cmd.Process.Pid)
		}
	}
	if _, err := os.Stat(pm.helperPath); err != nil {
		return 0, fmt.Errorf("找不到 helper.exe: %s", pm.helperPath)
	}
	cmd := exec.Command(pm.helperPath, "-config", pm.cfgPath)
	cmd.Dir = filepath.Dir(pm.cfgPath)
	// CREATE_NEW_PROCESS_GROUP keeps the child out of our console group
	// so Ctrl+C on the GUI console doesn't cascade into the helper
	// before our own shutdown handler can tree-kill it cleanly.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	cmd.Stderr = cmd.Stdout // merged into the same pipe → same ring
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pm.cmd = cmd
	pm.exitCode = nil
	pid := cmd.Process.Pid
	pm.ring.Append("system", fmt.Sprintf("helper 启动 (PID %d)", pid))

	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 256*1024)
		for sc.Scan() {
			pm.ring.Append("stdout", sc.Text())
		}
		if err := sc.Err(); err != nil {
			// The pipe closing with the process is the normal exit path
			// ("file already closed"); anything else must be visible.
			pm.ring.Append("system", "读取 helper 输出中断: "+err.Error())
		}
	}()
	go func() {
		err := cmd.Wait()
		code := 0
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else {
				code = -1
			}
		}
		pm.mu.Lock()
		pm.exitCode = &code
		pm.cmd = nil
		pm.mu.Unlock()
		pm.ring.Append("system", fmt.Sprintf("helper 退出 (exit code %d)", code))
	}()
	return pid, nil
}

// Stop tree-kills the child (taskkill /T /F — the project's sanctioned
// idiom; helper's exec children must die with it) and waits up to
// timeout for the reaper to finish.
func (pm *ProcManager) Stop(timeout time.Duration) (int, error) {
	pm.mu.Lock()
	cmd := pm.cmd
	pm.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return 0, fmt.Errorf("helper 未在运行")
	}
	pid := cmd.Process.Pid
	kill := exec.Command("taskkill", "/PID", fmt.Sprint(pid), "/T", "/F")
	if out, err := kill.CombinedOutput(); err != nil {
		return 0, fmt.Errorf("taskkill: %v: %s", err, string(out))
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pm.mu.Lock()
		done := pm.cmd == nil
		code := pm.exitCode
		pm.mu.Unlock()
		if done {
			if code == nil {
				return 0, nil
			}
			return *code, nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return 0, fmt.Errorf("helper 未在 %s 内退出", timeout)
}

// Status reports the helper's running state. When we have no child it
// still checks the system for a foreign helper.exe so the UI can switch
// to external mode (stop unavailable; log pane tails helper.log).
func (pm *ProcManager) Status() ProcState {
	pm.mu.Lock()
	cmd := pm.cmd
	code := pm.exitCode
	pm.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		return ProcState{Running: true, Source: SourceGUI, PID: cmd.Process.Pid}
	}
	st := ProcState{Running: false, Source: SourceNone}
	if code != nil {
		st.ExitCode = code
	}
	if pids, err := findExternalHelper(); err == nil && len(pids) > 0 {
		st.Source = SourceExternal
		st.Running = true
		st.PID = pids[0]
	}
	return st
}

// Shutdown is the signal-handler path: best-effort stop of our child.
func (pm *ProcManager) Shutdown() {
	_, _ = pm.Stop(8 * time.Second)
}

// LogPath returns the audit log path helper writes (config directory +
// log_file from the saved doc, default helper.log) — the log source in
// external mode.
func (pm *ProcManager) LogPath() string {
	doc, exists, err := (&ConfigStore{Path: pm.cfgPath}).LoadDoc()
	if err == nil && exists {
		if lf, ok := doc["log_file"].(string); ok && lf != "" {
			return absNextTo(filepath.Dir(pm.cfgPath), lf)
		}
	}
	return filepath.Join(filepath.Dir(pm.cfgPath), "helper.log")
}

// findExternalHelper lists PIDs of any helper.exe processes on the box
// (Windows-only; nil on other platforms). Cheap enough for 2s polling.
func findExternalHelper() ([]int, error) {
	out, err := exec.Command("tasklist", "/FO", "CSV", "/NH", "/FI", "IMAGENAME eq helper.exe").Output()
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, line := range splitLines(string(out)) {
		fields := csvSplit(line)
		if len(fields) < 2 || !strings.EqualFold(fields[0], "helper.exe") {
			continue
		}
		var pid int
		if _, err := fmt.Sscanf(fields[1], "%d", &pid); err == nil && pid != os.Getpid() {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			line := s[start:i]
			line = trimCR(line)
			if line != "" {
				lines = append(lines, line)
			}
			start = i + 1
		}
	}
	if start < len(s) {
		if line := trimCR(s[start:]); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func trimCR(s string) string {
	for len(s) > 0 && s[len(s)-1] == '\r' {
		s = s[:len(s)-1]
	}
	return s
}

// csvSplit splits one tasklist CSV line respecting double quotes.
func csvSplit(line string) []string {
	var fields []string
	var cur []byte
	inQ := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '"':
			inQ = !inQ
		case c == ',' && !inQ:
			fields = append(fields, string(cur))
			cur = cur[:0]
		default:
			cur = append(cur, c)
		}
	}
	fields = append(fields, string(cur))
	return fields
}
