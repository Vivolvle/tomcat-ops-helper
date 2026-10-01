package actions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"tomcat-ops-helper/internal/config"
	"tomcat-ops-helper/internal/execshell"
)

// StatusData is the JSON returned by the "status" action.
type StatusData struct {
	Hostname     string        `json:"hostname"`
	OS           string        `json:"os"`
	Now          time.Time     `json:"now"`
	TomcatHome   string        `json:"tomcat_home"`
	TomcatExists bool          `json:"tomcat_exists"`
	JavaProcs    []JavaProcess `json:"java_processes"`
	Ports        []Port        `json:"listening_ports"`
	// JavaProbeError / PortsProbeError carry the reason a probe came back
	// empty (e.g. wmic and powershell both unavailable) instead of failing
	// the whole action — the rest of the snapshot is still useful
	// (2026-09-25 report: silent nulls are undebuggable).
	JavaProbeError  string `json:"java_probe_error,omitempty"`
	PortsProbeError string `json:"ports_probe_error,omitempty"`
	// ConfigLoadedAt is when helper last read config.json. Compare against
	// the file's mtime on disk: a mismatch means config.json was edited
	// after startup and a restart is required for changes to take effect
	// (helper does not hot-reload config).
	ConfigLoadedAt time.Time `json:"config_loaded_at"`
}

func (r *Registry) status(ctx context.Context) (json.RawMessage, error) {
	procs, javaErr := r.probe.JavaProcesses(ctx)
	filtered := procs[:0]
	for _, p := range procs {
		if isInteresting(p, r.cfg.TomcatHome) {
			filtered = append(filtered, p)
		}
	}
	ports, portsErr := r.probe.ListeningPorts(ctx)
	host, _ := os.Hostname()
	_, statErr := os.Stat(r.cfg.TomcatHome)
	data := StatusData{
		Hostname:       host,
		OS:             runtime.GOOS,
		Now:            time.Now().UTC(),
		TomcatHome:     r.cfg.TomcatHome,
		TomcatExists:   statErr == nil,
		JavaProcs:      filtered,
		Ports:          ports,
		ConfigLoadedAt: r.cfg.LoadedAt.UTC(),
	}
	if javaErr != nil {
		data.JavaProbeError = fmt.Sprintf("status: java processes: %v", javaErr)
	}
	if portsErr != nil {
		data.PortsProbeError = fmt.Sprintf("status: ports: %v", portsErr)
	}
	return mustMarshal(data), nil
}

func isInteresting(p JavaProcess, tomcatHome string) bool {
	// Never treat the helper itself as a catalina process: on dev boxes the
	// helper runs from ...\tomcat-ops-helper\bin\helper.exe and the broad
	// name/cmdline match below would otherwise list it.
	if p.PID == os.Getpid() {
		return false
	}
	lc := strings.ToLower(p.Cmdline)
	if lc == "" {
		// prunsrv-hosted JVMs may only be identifiable by image name
		// (e.g. tomcat10-esb.exe) when CommandLine comes back empty.
		lc = strings.ToLower(p.Name)
	}
	if strings.Contains(lc, "tomcat") {
		return true
	}
	if strings.Contains(lc, "catalina") {
		return true
	}
	// Also check for Tomcat's main class and common JVM arguments
	if strings.Contains(lc, "org.apache.catalina.startup.bootstrap") {
		return true
	}
	if strings.Contains(lc, "-Dcatalina.home=") || strings.Contains(lc, "-Dcatalina.base=") {
		return true
	}
	if tomcatHome != "" && strings.Contains(lc, strings.ToLower(filepath.ToSlash(tomcatHome))) {
		return true
	}
	return false
}

// LogFile describes one entry returned by list_logs. Subdirectories are
// included too (todo H2): Type distinguishes "dir" from "file" so callers
// can descend without a separate listing tool.
type LogFile struct {
	Name    string    `json:"name"`
	Type    string    `json:"type"` // "file" | "dir"
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

func (r *Registry) listLogs(params json.RawMessage) (json.RawMessage, error) {
	p, err := decodeParams(params)
	if err != nil {
		return nil, err
	}
	dir := r.cfg.LogDir
	if v, ok := p["dir"].(string); ok && v != "" {
		dir = v
	}
	if !r.cfg.IsAllowedWorkDir(dir) {
		return nil, fmt.Errorf("list_logs: dir not allowed: %s", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("list_logs: %w", err)
	}
	files := make([]LogFile, 0, len(entries))
	for _, e := range entries {
		kind := "file"
		if e.IsDir() {
			kind = "dir"
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, LogFile{Name: e.Name(), Type: kind, Size: info.Size(), ModTime: info.ModTime().UTC()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].ModTime.After(files[j].ModTime) })
	return mustMarshal(map[string]any{"dir": dir, "files": files}), nil
}

// TailData is the JSON returned by "tail".
type TailData struct {
	File  string   `json:"file"`
	Lines int      `json:"lines"`
	Body  []string `json:"body"`
}

func (r *Registry) tail(params json.RawMessage) (json.RawMessage, error) {
	p, err := decodeParams(params)
	if err != nil {
		return nil, err
	}
	file, err := requireString(p, "file")
	if err != nil {
		return nil, fmt.Errorf("tail: %w", err)
	}
	linesF, ok := p["lines"].(float64)
	if !ok || linesF <= 0 {
		linesF = 200
	}
	if linesF > 5000 {
		linesF = 5000
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		return nil, fmt.Errorf("tail: %w", err)
	}
	if !r.cfg.IsAllowedWorkDir(filepath.Dir(abs)) {
		return nil, fmt.Errorf("tail: file dir not allowed")
	}
	body, err := readFileTail(abs, int(linesF))
	if err != nil {
		return nil, fmt.Errorf("tail: %w", err)
	}
	return mustMarshal(TailData{File: abs, Lines: len(body), Body: body}), nil
}

// StopServiceData is the JSON returned by "stop_service".
type StopServiceData struct {
	ServiceName string           `json:"service_name"`
	Stop        execshell.Result `json:"stop"`
	Exited      bool             `json:"exited"` // true if catalina processes confirmed gone
	// ExitWait explains why Exited=false (wait timed out, or JVM
	// enumeration unavailable and the service state could not confirm
	// the exit). Informational only — the stop itself succeeded.
	ExitWait string `json:"exit_wait,omitempty"`
}

func (r *Registry) stopService(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	p, err := decodeParams(params)
	if err != nil {
		return nil, err
	}
	timeout := r.cfg.CommandTimeout
	if v, ok := p["timeout"].(float64); ok && v > 0 {
		timeout = time.Duration(v * float64(time.Second))
	}
	serviceName := r.discoverWindowsServiceName(ctx)
	if serviceName == "" {
		return nil, fmt.Errorf("stop_service: cannot determine Windows service name")
	}
	data := StopServiceData{ServiceName: serviceName}
	stopCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	data.Stop, err = r.executor.Exec(stopCtx, fmt.Sprintf("sc stop %q", serviceName), r.cfg.TomcatHome)
	if err != nil || (data.Stop.ExitCode != 0 && !strings.Contains(strings.ToLower(data.Stop.Stdout+data.Stop.Stderr), "1062")) {
		return nil, fmt.Errorf("stop_service: service %s did not accept stop: %s", serviceName, resultMessage(data.Stop, err))
	}
	// Wait for catalina processes to actually exit
	waitTimeout := timeout
	if v, ok := p["wait_timeout"].(float64); ok && v > 0 {
		waitTimeout = time.Duration(v * float64(time.Second))
	}
	if err := r.waitForCatalinaExitWithTimeout(stopCtx, waitTimeout, serviceName); err == nil {
		data.Exited = true
	} else {
		// Non-fatal: catalina may linger (timedOut) or JVM enumeration
		// may be unavailable on this box (fell back to sc query). The
		// stop itself succeeded — the caller decides whether to proceed.
		data.Exited = false
		if perr, ok := err.(*waitTimeoutError); ok {
			data.ExitWait = perr.Error()
		} else {
			data.ExitWait = err.Error()
		}
	}
	return mustMarshal(data), nil
}

// waitTimeoutError is a sentinel for when waitForCatalinaExit times out.
type waitTimeoutError struct {
	timedOut bool
}

func (e *waitTimeoutError) Error() string {
	return "catalina processes still running after timeout"
}

// waitForCatalinaExitWithTimeout waits for catalina processes to exit,
// returning a waitTimeoutError if they don't exit in time (non-fatal).
//
// When JVM enumeration is unavailable (probe errors — 2026-09-25 report:
// WIN-JRD8SLQTK01's WMI is sick and every strategy errored), we no longer
// fail fast or wait blind: sc query takes over as the authority. A service
// in STOPPED state means the SCM took the JVM down with it — good enough
// to proceed. Only when BOTH signals are unavailable do we poll until the
// deadline and return the (non-fatal) timeout.
func (r *Registry) waitForCatalinaExitWithTimeout(ctx context.Context, timeout time.Duration, serviceName string) error {
	pollCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		procs, err := r.probe.JavaProcesses(pollCtx)
		if err != nil {
			// Probe unavailable: fall back to the service state. Empty
			// serviceName (service discovery failed) keeps polling —
			// bounded by the deadline below either way.
			if serviceName != "" {
				if state, serr := r.queryServiceState(pollCtx, serviceName); serr == nil && state == "STOPPED" {
					return nil
				}
			}
		} else {
			found := false
			for _, p := range procs {
				if isInteresting(p, r.cfg.TomcatHome) {
					found = true
					break
				}
			}
			if !found {
				return nil
			}
		}
		select {
		case <-pollCtx.Done():
			return &waitTimeoutError{timedOut: true}
		case <-time.After(time.Second):
		}
	}
}

// StartServiceData is the JSON returned by "start_service".
type StartServiceData struct {
	ServiceName string           `json:"service_name"`
	Start       execshell.Result `json:"start"`
	Running     bool             `json:"running"` // true if service confirmed RUNNING
	// AlreadyRunning is set when sc start returned 1056 ("An instance of
	// the service is already running") and a state re-check confirmed the
	// service RUNNING — a racing start (e.g. a deploy task in the
	// background, 2026-09-25 report) is success, not an error.
	AlreadyRunning bool   `json:"already_running,omitempty"`
	Note           string `json:"note,omitempty"`
}

func (r *Registry) startService(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	p, err := decodeParams(params)
	if err != nil {
		return nil, err
	}
	timeout := r.cfg.CommandTimeout
	if v, ok := p["timeout"].(float64); ok && v > 0 {
		timeout = time.Duration(v * float64(time.Second))
	}
	serviceName := r.discoverWindowsServiceName(ctx)
	if serviceName == "" {
		return nil, fmt.Errorf("start_service: cannot determine Windows service name")
	}
	data := StartServiceData{ServiceName: serviceName}
	startCtx, startCancel := context.WithTimeout(ctx, timeout)
	defer startCancel()
	data.Start, err = r.executor.Exec(startCtx, fmt.Sprintf("sc start %q", serviceName), r.cfg.TomcatHome)
	if err != nil || data.Start.ExitCode != 0 {
		// 1056 = ERROR_SERVICE_ALREADY_RUNNING: someone else (a deploy
		// task, a slow SCM transition) started it first. Re-check the
		// actual state instead of failing — already RUNNING means the
		// caller's goal is achieved (2026-09-25 report).
		if data.Start.ExitCode == 1056 || strings.Contains(strings.ToLower(data.Start.Stdout+data.Start.Stderr), "1056") {
			state, serr := r.queryServiceState(startCtx, serviceName)
			switch {
			case serr == nil && state == "RUNNING":
				data.Running = true
				data.AlreadyRunning = true
				data.Note = "sc start returned 1056 (already running); state re-check confirmed RUNNING"
				return mustMarshal(data), nil
			case serr == nil && (state == "START_PENDING" || state == "CONTINUE_PENDING"):
				if werr := r.waitServiceState(startCtx, serviceName, "RUNNING"); werr == nil {
					data.Running = true
					data.AlreadyRunning = true
					data.Note = "sc start returned 1056; service was START_PENDING and reached RUNNING"
					return mustMarshal(data), nil
				}
			}
			return nil, fmt.Errorf("start_service: sc start returned 1056 but service %s is not RUNNING (state=%s, err=%v)", serviceName, state, serr)
		}
		return nil, fmt.Errorf("start_service: service %s failed to start: %s", serviceName, resultMessage(data.Start, err))
	}
	if err := r.waitServiceState(startCtx, serviceName, "RUNNING"); err != nil {
		return nil, fmt.Errorf("start_service: service %s did not reach RUNNING within %s (sc start was accepted; the JVM may still be booting — check `sc query` / catalina log): %w", serviceName, timeout, err)
	}
	data.Running = true
	return mustMarshal(data), nil
}

// WaitCatalinaData is the JSON returned by "wait_for_catalina".
type WaitCatalinaData struct {
	ServiceName string `json:"service_name,omitempty"`
	Wanted      string `json:"wanted"`
	Exited      bool   `json:"exited"`  // true if catalina processes gone
	Running     bool   `json:"running"` // true if catalina processes found
	TimedOut    bool   `json:"timed_out"`
	// Note explains a fallback confirmation (e.g. "confirmed via sc
	// query" when JVM enumeration is unavailable on this box).
	Note string `json:"note,omitempty"`
}

func (r *Registry) waitCatalina(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	p, err := decodeParams(params)
	if err != nil {
		return nil, err
	}
	timeout := r.cfg.CommandTimeout
	if v, ok := p["timeout"].(float64); ok && v > 0 {
		timeout = time.Duration(v * float64(time.Second))
	}
	state, _ := p["state"].(string)
	if state != "gone" && state != "running" {
		state = "gone" // default
	}
	data := WaitCatalinaData{Wanted: state}
	serviceName := r.discoverWindowsServiceName(ctx)
	if serviceName != "" {
		data.ServiceName = serviceName
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		procs, err := r.probe.JavaProcesses(waitCtx)
		if err != nil {
			// JVM enumeration unavailable on this box (2026-09-25 report:
			// WMI sick on WIN-JRD8SLQTK01). For state=gone the service
			// state is an acceptable authority: sc query STOPPED ⇒ the
			// SCM took the JVM down. For state=running we cannot confirm
			// without enumeration — fall through to the deadline and
			// report the probe failure instead of a blind wait.
			if state == "gone" && serviceName != "" {
				if s, serr := r.queryServiceState(waitCtx, serviceName); serr == nil && s == "STOPPED" {
					data.Exited = true
					data.Note = "confirmed via sc query (JVM enumeration unavailable: " + err.Error() + ")"
					return mustMarshal(data), nil
				}
			}
			if state == "running" {
				return nil, fmt.Errorf("wait_for_catalina: probe: %w", err)
			}
		} else {
			found := false
			for _, pr := range procs {
				if isInteresting(pr, r.cfg.TomcatHome) {
					found = true
					break
				}
			}
			if state == "gone" && !found {
				data.Exited = true
				return mustMarshal(data), nil
			}
			if state == "running" && found {
				data.Running = true
				return mustMarshal(data), nil
			}
		}
		select {
		case <-waitCtx.Done():
			data.TimedOut = true
			if state == "gone" {
				return mustMarshal(data), nil // return partial result, not error
			}
			return nil, fmt.Errorf("wait_for_catalina: timed out waiting for state %s", state)
		case <-time.After(time.Second):
		}
	}
}

// DeployData is the JSON returned by "deploy".
type DeployData struct {
	ServiceName   string            `json:"service_name,omitempty"`
	StopResult    *StopServiceData  `json:"stop_result,omitempty"`
	ReplaceResult *ReplaceJarData   `json:"replace_result,omitempty"`
	StartResult   *StartServiceData `json:"start_result,omitempty"`
	// StartFallback is the best-effort retry issued when the start step
	// failed, so a deploy never leaves the service silently stopped
	// (2026-09-25 report). FallbackError explains why the retry (also)
	// failed; FinalState is the last observed SCM state.
	StartFallback *StartServiceData `json:"start_fallback,omitempty"`
	FallbackError string            `json:"fallback_error,omitempty"`
	FinalState    string            `json:"final_state,omitempty"`
	Error         string            `json:"error,omitempty"`
	Step          string            `json:"step"` // which step failed / is running
	// TaskID ties this deploy to its task_status entry. Present on every
	// reply since every deploy — synchronous or async — is tracked.
	TaskID string `json:"task_id,omitempty"`
}

func (r *Registry) deploy(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	p, err := decodeParams(params)
	if err != nil {
		return nil, err
	}
	src, _ := p["src"].(string)
	dst, _ := p["dest"].(string)
	restart := true
	if v, ok := p["restart"].(bool); ok {
		restart = v
	}
	// A full stop→replace→start cycle on a real box routinely exceeds
	// cfg.CommandTimeout (30s default): catalina shutdown alone can take
	// tens of seconds and Tomcat boots 1-2 minutes with big apps. Floor the
	// per-step defaults well above CommandTimeout instead of inheriting it
	// (2026-09-25 report: deploy timed out mid-cycle and left the service
	// stopped).
	stopTimeout := maxDuration(r.cfg.CommandTimeout, 60*time.Second)
	if v, ok := p["stop_timeout"].(float64); ok && v > 0 {
		stopTimeout = time.Duration(v * float64(time.Second))
	}
	startTimeout := maxDuration(r.cfg.CommandTimeout, 180*time.Second)
	if v, ok := p["start_timeout"].(float64); ok && v > 0 {
		startTimeout = time.Duration(v * float64(time.Second))
	}
	async := false
	if v, ok := p["async"].(bool); ok {
		async = v
	}

	core := func(wctx context.Context, data *DeployData, notify func()) {
		r.deployCore(wctx, data, notify, src, dst, restart, stopTimeout, startTimeout)
	}
	t := r.runDeployTask(ctx, core)
	if async {
		// Caller knows the cycle outlives its tool timeout: hand back a
		// task handle right away instead of a doomed synchronous wait.
		return mustMarshal(map[string]any{
			"task_id": t.ID,
			"tool":    "deploy",
			"status":  "running",
			"hint":    "poll task_status with this task_id for step-level progress (stop_service → replace_jar → start_service)",
		}), nil
	}
	select {
	case <-t.doneCh:
		data, _, _ := t.snapshot()
		return mustMarshal(data), nil
	case <-ctx.Done():
		// The caller's transport is going away mid-deploy. Hand back the
		// running task id instead of a dead timeout — the worker keeps
		// going and task_status tells the caller where it is.
		partial, _, _ := t.snapshot()
		return mustMarshal(map[string]any{
			"task_id": t.ID,
			"tool":    "deploy",
			"status":  "still_running",
			"step":    partial.Step,
			"hint":    "caller context expired before deploy finished; poll task_status with this task_id",
		}), nil
	}
}

// deployCore is the synchronous stop→replace→start cycle. It mutates
// data in place and calls notify after every step so the wrapping task
// publishes fresh snapshots for task_status polling. It never returns an
// error — step failures are recorded on data and the cycle stops there
// (callers get the partial picture, matching the pre-task contract).
func (r *Registry) deployCore(ctx context.Context, data *DeployData, notify func(), src, dst string, restart bool, stopTimeout, startTimeout time.Duration) {
	serviceName := r.discoverWindowsServiceName(ctx)
	if serviceName != "" {
		data.ServiceName = serviceName
	}
	notify()

	// finish stamps the final observed service state. Every exit path
	// goes through it so the caller always learns where the service
	// ended up.
	finish := func() {
		if serviceName != "" {
			stateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			if state, err := r.queryServiceState(stateCtx, serviceName); err == nil {
				data.FinalState = state
			}
		}
		notify()
	}

	// Step 1: stop the service
	stopCtx, cancel := context.WithTimeout(ctx, stopTimeout)
	defer cancel()
	stopRaw, stopErr := r.stopService(stopCtx, mustMarshal(map[string]any{"timeout": float64(stopTimeout.Seconds()), "wait_timeout": float64(stopTimeout.Seconds())}))
	if stopErr != nil {
		data.Error = stopErr.Error()
		data.Step = "stop_service"
		finish()
		return
	}
	var stopData StopServiceData
	if jsonErr := json.Unmarshal(stopRaw, &stopData); jsonErr == nil {
		data.StopResult = &stopData
	}
	notify()

	// Step 2: replace jar if src/dest provided
	if restart && src != "" && dst != "" {
		data.Step = "replace_jar"
		notify()
		replaceRaw, replaceErr := r.replaceJar(ctx, mustMarshal(map[string]any{"src": src, "dest": dst, "restart": false}))
		if replaceErr != nil {
			data.Error = replaceErr.Error()
			data.Step = "replace_jar"
			finish()
			return
		}
		var replaceData ReplaceJarData
		if jsonErr := json.Unmarshal(replaceRaw, &replaceData); jsonErr == nil {
			data.ReplaceResult = &replaceData
		}
		notify()
	}

	// Step 3: start the service
	startCtx, startCancel := context.WithTimeout(ctx, startTimeout)
	defer startCancel()
	data.Step = "start_service"
	notify()
	startRaw, startErr := r.startService(startCtx, mustMarshal(map[string]any{"timeout": float64(startTimeout.Seconds())}))
	if startErr != nil {
		data.Error = startErr.Error()
		data.Step = "start_service"
		// Fallback: never leave the box in "stopped, gave up" without one
		// more attempt. The retry is state-aware — if the service is
		// already RUNNING or still START_PENDING we must not blindly
		// re-issue sc start ("instance already running").
		fbCtx, fbCancel := context.WithTimeout(context.WithoutCancel(ctx), startTimeout)
		defer fbCancel()
		fb, fbErr := r.fallbackStart(fbCtx, serviceName, startTimeout)
		if fb != nil {
			data.StartFallback = fb
		}
		if fbErr != "" {
			data.FallbackError = fbErr
		}
		finish()
		return
	}
	var startData StartServiceData
	if jsonErr := json.Unmarshal(startRaw, &startData); jsonErr == nil {
		data.StartResult = &startData
	}

	finish()
}

// fallbackStart makes one state-aware best-effort attempt to leave the
// Windows service RUNNING after a failed start step. It returns the
// StartServiceData of the attempt (nil when it could not even try) and an
// error string explaining the outcome.
func (r *Registry) fallbackStart(ctx context.Context, serviceName string, timeout time.Duration) (*StartServiceData, string) {
	if serviceName == "" {
		return nil, "cannot determine Windows service name for fallback start"
	}
	state, err := r.queryServiceState(ctx, serviceName)
	if err != nil {
		return nil, "fallback: " + err.Error()
	}
	switch state {
	case "RUNNING":
		return &StartServiceData{ServiceName: serviceName, Running: true}, ""
	case "START_PENDING", "CONTINUE_PENDING":
		if werr := r.waitServiceState(ctx, serviceName, "RUNNING"); werr == nil {
			return &StartServiceData{ServiceName: serviceName, Running: true}, ""
		}
		return &StartServiceData{ServiceName: serviceName},
			fmt.Sprintf("fallback: service still %s after %s", state, timeout)
	default: // STOPPED / STOP_PENDING / PAUSED → safe to (re)start
		raw, serr := r.startService(ctx, mustMarshal(map[string]any{"timeout": float64(timeout.Seconds())}))
		if serr != nil {
			return nil, "fallback start: " + serr.Error()
		}
		var d StartServiceData
		if jsonErr := json.Unmarshal(raw, &d); jsonErr != nil {
			return &StartServiceData{ServiceName: serviceName}, ""
		}
		return &d, ""
	}
}

// queryServiceState parses the STATE line out of `sc query` output, e.g.
// "STATE              : 4  RUNNING" → "RUNNING".
func (r *Registry) queryServiceState(ctx context.Context, serviceName string) (string, error) {
	q, err := r.executor.Exec(ctx, fmt.Sprintf("sc query %q", serviceName), r.cfg.TomcatHome)
	if err != nil {
		return "", fmt.Errorf("sc query %s: %w", serviceName, err)
	}
	if q.ExitCode != 0 {
		return "", fmt.Errorf("sc query %s: exit_code=%d %s", serviceName, q.ExitCode, strings.TrimSpace(q.Stdout+" "+q.Stderr))
	}
	re := regexp.MustCompile(`(?im)^\s*STATE\s*:\s*\d+\s+(\S+)`)
	m := re.FindStringSubmatch(q.Stdout + "\n" + q.Stderr)
	if m == nil {
		return "", fmt.Errorf("sc query %s: no STATE line in output", serviceName)
	}
	return strings.ToUpper(m[1]), nil
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

// RestartData is the JSON returned by "restart".
type RestartData struct {
	Mode        string           `json:"mode"`
	ServiceName string           `json:"service_name,omitempty"`
	Shutdown    execshell.Result `json:"shutdown"`
	Startup     execshell.Result `json:"startup"`
	Exited      bool             `json:"exited"` // catalina confirmed exited
}

func (r *Registry) restart(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	p, err := decodeParams(params)
	if err != nil {
		return nil, err
	}
	stopTimeout := r.cfg.CommandTimeout
	if v, ok := p["stop_timeout"].(float64); ok && v > 0 {
		stopTimeout = time.Duration(v * float64(time.Second))
	}
	startTimeout := stopTimeout
	if v, ok := p["start_timeout"].(float64); ok && v > 0 {
		startTimeout = time.Duration(v * float64(time.Second))
	}
	if runtime.GOOS == "windows" {
		serviceName := r.discoverWindowsServiceName(ctx)
		if data, handled, err := r.restartWindowsService(ctx, serviceName, stopTimeout, startTimeout); handled {
			if err != nil {
				return nil, err
			}
			return mustMarshal(data), nil
		}
	}
	bin := filepath.Join(r.cfg.TomcatHome, "bin")
	if runtime.GOOS == "windows" {
		bin = filepath.Join(bin, "shutdown.bat")
	} else {
		bin = filepath.Join(bin, "shutdown.sh")
	}
	if !r.cfg.IsAllowedWorkDir(bin) {
		return nil, fmt.Errorf("restart: shutdown script not in allowed dir")
	}
	workDir := r.cfg.TomcatHome

	downCtx, cancel := context.WithTimeout(ctx, stopTimeout)
	defer cancel()
	shutdown, _ := r.executor.Exec(downCtx, binQuote(bin, runtime.GOOS), workDir)

	// Give the JVM a beat to release ports.
	select {
	case <-time.After(2 * time.Second):
	case <-ctx.Done():
	}

	up := bin
	if runtime.GOOS == "windows" {
		up = filepath.Join(r.cfg.TomcatHome, "bin", "startup.bat")
	} else {
		up = filepath.Join(r.cfg.TomcatHome, "bin", "startup.sh")
	}
	upCtx, upCancel := context.WithTimeout(ctx, startTimeout)
	defer upCancel()
	startup, _ := r.executor.Exec(upCtx, binQuote(up, runtime.GOOS), workDir)

	return mustMarshal(RestartData{Mode: "scripts", Shutdown: shutdown, Startup: startup}), nil
}

// discoverWindowsServiceName prefers the configured name, then looks through
// SCM entries for a service whose binary path references this Tomcat home.
// This covers names such as "tomcat10-esb" that cannot be inferred from the
// HTTP connector port. The server.xml-derived name remains a final fallback.
func (r *Registry) discoverWindowsServiceName(ctx context.Context) string {
	if r.cfg.ServiceName != "" {
		return r.cfg.ServiceName
	}
	listing, err := r.executor.Exec(ctx, "sc query type= service state= all", r.cfg.TomcatHome)
	if err == nil && listing.ExitCode == 0 {
		re := regexp.MustCompile(`(?im)^SERVICE_NAME:\s*(\S+)\s*$`)
		want := strings.ToLower(filepath.Clean(r.cfg.TomcatHome))
		for _, match := range re.FindAllStringSubmatch(listing.Stdout, -1) {
			name := match[1]
			qc, qcErr := r.executor.Exec(ctx, fmt.Sprintf("sc qc %q", name), r.cfg.TomcatHome)
			if qcErr != nil || qc.ExitCode != 0 {
				continue
			}
			text := strings.ToLower(qc.Stdout + "\n" + qc.Stderr)
			if strings.Contains(text, want) && (strings.Contains(text, "//rs//") || strings.Contains(text, "tomcat")) {
				return name
			}
		}
	}
	return resolveServiceName(r.cfg.TomcatHome, "")
}

// restartWindowsService uses SCM for Tomcat installations registered through
// Apache Commons Daemon/procrun. Running shutdown.bat against such an install
// can exit 0 without stopping the service (and may even use a different JRE).
func (r *Registry) restartWindowsService(ctx context.Context, serviceName string, stopTimeout, startTimeout time.Duration) (RestartData, bool, error) {
	workDir := r.cfg.TomcatHome
	qc, err := r.executor.Exec(ctx, fmt.Sprintf("sc qc %q", serviceName), workDir)
	if err != nil || qc.ExitCode != 0 {
		return RestartData{}, false, nil // not registered: retain script fallback
	}
	qcText := strings.ToLower(qc.Stdout + "\n" + qc.Stderr)
	if !strings.Contains(qcText, "//rs//") && !strings.Contains(qcText, "tomcat") {
		return RestartData{}, false, nil
	}

	data := RestartData{Mode: "windows_service", ServiceName: serviceName}
	stopCtx, cancel := context.WithTimeout(ctx, stopTimeout)
	defer cancel()
	data.Shutdown, err = r.executor.Exec(stopCtx, fmt.Sprintf("sc stop %q", serviceName), workDir)
	if err != nil || (data.Shutdown.ExitCode != 0 && !strings.Contains(strings.ToLower(data.Shutdown.Stdout+data.Shutdown.Stderr), "1062")) {
		return data, true, fmt.Errorf("restart: service %s did not accept stop; use sc stop %s manually: %s", serviceName, serviceName, resultMessage(data.Shutdown, err))
	}
	if err := r.waitServiceState(stopCtx, serviceName, "STOPPED"); err != nil {
		return data, true, fmt.Errorf("restart: service %s was not stopped within %s; consider taskkill /F /PID after checking sc queryex %s: %w", serviceName, stopTimeout, serviceName, err)
	}

	// Wait for catalina processes to actually exit after service stops
	if err := r.waitForCatalinaExitWithTimeout(stopCtx, stopTimeout, serviceName); err != nil {
		// Non-fatal: catalina may linger but we can still try to start
	}

	startCtx, startCancel := context.WithTimeout(ctx, startTimeout)
	defer startCancel()
	data.Startup, err = r.executor.Exec(startCtx, fmt.Sprintf("sc start %q", serviceName), workDir)
	if err != nil || data.Startup.ExitCode != 0 {
		return data, true, fmt.Errorf("restart: service %s failed to start: %s", serviceName, resultMessage(data.Startup, err))
	}
	if err := r.waitServiceState(startCtx, serviceName, "RUNNING"); err != nil {
		return data, true, fmt.Errorf("restart: service %s did not reach RUNNING within %s: %w", serviceName, startTimeout, err)
	}
	return data, true, nil
}

func (r *Registry) waitServiceState(ctx context.Context, serviceName, wanted string) error {
	for {
		q, err := r.executor.Exec(ctx, fmt.Sprintf("sc query %q", serviceName), r.cfg.TomcatHome)
		if err != nil {
			return err
		}
		text := strings.ToUpper(q.Stdout + "\n" + q.Stderr)
		if q.ExitCode == 0 && strings.Contains(text, wanted) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func resultMessage(res execshell.Result, err error) string {
	if err != nil {
		return err.Error()
	}
	msg := strings.TrimSpace(res.Stderr)
	if msg == "" {
		msg = strings.TrimSpace(res.Stdout)
	}
	return fmt.Sprintf("exit_code=%d %s", res.ExitCode, msg)
}

// binQuote wraps a script path with quotes suitable for the platform shell.
// We use double quotes because Tomcat paths rarely contain literal quotes.
func binQuote(p string, goos string) string {
	q := `"` + strings.ReplaceAll(p, `"`, `\"`) + `"`
	if goos == "windows" {
		// cmd.exe needs surrounding double quotes when path has spaces.
		return q
	}
	return q
}

// ReplaceJarData is the JSON returned by "replace_jar".
type ReplaceJarData struct {
	Source      string       `json:"source"`
	Dest        string       `json:"dest"`
	SHA256      string       `json:"sha256"`
	Bytes       int64        `json:"bytes"`
	Restarted   bool         `json:"restarted,omitempty"`
	RestartData *RestartData `json:"restart_data,omitempty"`
	RestartErr  string       `json:"restart_error,omitempty"`
}

func (r *Registry) replaceJar(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	p, err := decodeParams(params)
	if err != nil {
		return nil, err
	}
	src, err := requireString(p, "src")
	if err != nil {
		return nil, fmt.Errorf("replace_jar: %w", err)
	}
	dst, err := requireString(p, "dest")
	if err != nil {
		return nil, fmt.Errorf("replace_jar: %w", err)
	}
	srcAbs, err := filepath.Abs(src)
	if err != nil {
		return nil, fmt.Errorf("replace_jar: src: %w", err)
	}
	dstAbs, err := filepath.Abs(dst)
	if err != nil {
		return nil, fmt.Errorf("replace_jar: dest: %w", err)
	}
	if !r.cfg.IsAllowedWorkDir(srcAbs) {
		return nil, fmt.Errorf("replace_jar: src not in allowed dir")
	}
	if !r.cfg.IsAllowedWorkDir(dstAbs) {
		return nil, fmt.Errorf("replace_jar: dest not in allowed dir")
	}
	// Backup existing dest if it exists.
	bak := dstAbs + ".bak"
	if _, err := os.Stat(dstAbs); err == nil {
		if err := os.Rename(dstAbs, bak); err != nil {
			return nil, fmt.Errorf("replace_jar: backup: %w", err)
		}
	}
	if err := copyFile(srcAbs, dstAbs); err != nil {
		// Try to restore the backup so we don't leave a half-baked app.
		if _, statErr := os.Stat(bak); statErr == nil {
			_ = os.Rename(bak, dstAbs)
		}
		return nil, fmt.Errorf("replace_jar: copy: %w", err)
	}
	sum, err := sha256Of(dstAbs)
	if err != nil {
		return nil, fmt.Errorf("replace_jar: hash: %w", err)
	}
	st, err := os.Stat(dstAbs)
	if err != nil {
		return nil, fmt.Errorf("replace_jar: stat: %w", err)
	}
	data := ReplaceJarData{Source: srcAbs, Dest: dstAbs, SHA256: sum, Bytes: st.Size()}
	if v, ok := p["restart"].(bool); ok && v {
		raw, err := r.restart(ctx, mustMarshal(map[string]any{"timeout": float64(r.cfg.CommandTimeout.Seconds())}))
		data.Restarted = true
		if err != nil {
			data.RestartErr = err.Error()
		} else if raw != nil {
			var restartData RestartData
			if jsonErr := json.Unmarshal(raw, &restartData); jsonErr == nil {
				data.RestartData = &restartData
			}
		}
	}
	return mustMarshal(data), nil
}

// KillZombieData is the JSON returned by "kill_zombie".
type KillZombieData struct {
	Killed []int    `json:"killed"`
	Found  []int    `json:"found"`
	Errors []string `json:"errors,omitempty"`
}

func (r *Registry) killZombie(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	p, err := decodeParams(params)
	if err != nil {
		return nil, err
	}
	pattern, _ := p["pattern"].(string)
	pidF, hasPID := p["pid"].(float64)

	procs, err := r.probe.JavaProcesses(ctx)
	if err != nil {
		return nil, fmt.Errorf("kill_zombie: probe: %w", err)
	}
	data := KillZombieData{}
	for _, jp := range procs {
		// When a specific PID is given, match it regardless of isInteresting —
		// the PID is authoritative (fixes problem.md #3).
		if hasPID && int(pidF) == jp.PID {
			data.Found = append(data.Found, jp.PID)
			if err := killPID(ctx, jp.PID); err != nil {
				data.Errors = append(data.Errors, fmt.Sprintf("pid=%d: %v", jp.PID, err))
				continue
			}
			data.Killed = append(data.Killed, jp.PID)
			continue
		}
		if !isInteresting(jp, r.cfg.TomcatHome) {
			continue
		}
		match := false
		if pattern != "" && strings.Contains(strings.ToLower(jp.Cmdline), strings.ToLower(pattern)) {
			match = true
		}
		if !match {
			continue
		}
		data.Found = append(data.Found, jp.PID)
		if err := killPID(ctx, jp.PID); err != nil {
			data.Errors = append(data.Errors, fmt.Sprintf("pid=%d: %v", jp.PID, err))
			continue
		}
		data.Killed = append(data.Killed, jp.PID)
	}
	return mustMarshal(data), nil
}

func killPID(ctx context.Context, pid int) error {
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "taskkill", "/F", "/PID", fmt.Sprintf("%d", pid)).Run()
	}
	return exec.CommandContext(ctx, "kill", "-9", fmt.Sprintf("%d", pid)).Run()
}

// KillPIDData is returned by the general-purpose kill_pid action.
type KillPIDData struct {
	PID    int              `json:"pid"`
	Force  bool             `json:"force"`
	Tree   bool             `json:"tree"`
	Result execshell.Result `json:"result"`
}

func (r *Registry) killProcess(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	p, err := decodeParams(params)
	if err != nil {
		return nil, fmt.Errorf("kill_pid: %w", err)
	}
	pidF, ok := p["pid"].(float64)
	pid := int(pidF)
	if !ok || pid < 1 || float64(pid) != pidF {
		return nil, fmt.Errorf("kill_pid: pid must be a positive integer")
	}
	if pid == os.Getpid() {
		return nil, fmt.Errorf("kill_pid: refusing to terminate the helper process itself (pid=%d)", pid)
	}
	force := true
	if v, exists := p["force"].(bool); exists {
		force = v
	}
	tree := runtime.GOOS == "windows"
	if v, exists := p["tree"].(bool); exists {
		tree = v
	}

	var cmd string
	if runtime.GOOS == "windows" {
		parts := []string{"taskkill"}
		if tree {
			parts = append(parts, "/T")
		}
		if force {
			parts = append(parts, "/F")
		}
		parts = append(parts, "/PID", strconv.Itoa(pid))
		cmd = strings.Join(parts, " ")
	} else {
		signal := "-TERM"
		if force {
			signal = "-KILL"
		}
		cmd = fmt.Sprintf("kill %s %d", signal, pid)
	}
	workDir, _ := r.resolveWorkDir("")
	res, execErr := r.executor.Exec(ctx, cmd, workDir)
	if execErr != nil {
		return nil, fmt.Errorf("kill_pid: %w", execErr)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("kill_pid: command failed (exit_code=%d): %s", res.ExitCode, strings.TrimSpace(res.Stderr+" "+res.Stdout))
	}
	return mustMarshal(KillPIDData{PID: pid, Force: force, Tree: tree, Result: res}), nil
}

// RegenServiceData is the JSON returned by "regen_service".
type RegenServiceData struct {
	Path           string `json:"path"`
	Bytes          int    `json:"bytes"`
	ServiceName    string `json:"service_name,omitempty"`
	PrunsrvPath    string `json:"prunsrv_path,omitempty"`   // path to <ServiceName>.exe (prunsrv)
	PrunsrvCopied  bool   `json:"prunsrv_copied"`           // true if tomcat9/tomcat10.exe was copied to <ServiceName>.exe
	PrunsrvExisted bool   `json:"prunsrv_already_existed"`  // true if <ServiceName>.exe was already on disk
	PrunsrvSource  string `json:"prunsrv_source,omitempty"` // where the prunsrv copy was sourced from, if anything
	MonitorPath    string `json:"monitor_path,omitempty"`   // path to <ServiceName>w.exe
	MonitorCopied  bool   `json:"monitor_copied"`           // true if tomcat9w/tomcat10w.exe was copied to <ServiceName>w.exe
	MonitorExisted bool   `json:"monitor_already_existed"`  // true if <ServiceName>w.exe was already on disk
	MonitorSource  string `json:"monitor_source,omitempty"` // where the monitor copy was sourced from, if anything
	Content        string `json:"content,omitempty"`
}

// resolveServiceName derives the Windows service name. cfg.ServiceName wins;
// otherwise we look for the first <Connector port="N"/> in server.xml and
// return "Tomcat<N>"; failing that we return "Tomcat".
func resolveServiceName(tomcatHome, configured string) string {
	if configured != "" {
		return configured
	}
	confPath := filepath.Join(tomcatHome, "conf", "server.xml")
	data, err := os.ReadFile(confPath)
	if err != nil {
		return "Tomcat"
	}
	// Coarse: find <Connector ... port="N" ...>. Not XML-aware; accepts
	// double or single quotes and runs of attributes. Good enough for the
	// canonical server.xml Tomcat ships.
	re := regexp.MustCompile(`(?i)<Connector[^>]*\bport\s*=\s*["'](\d+)["']`)
	m := re.FindSubmatch(data)
	if m == nil {
		return "Tomcat"
	}
	return "Tomcat" + string(m[1])
}

// resolvePrunsrvSource tries Tomcat 10 then 9. Returns the prunsrv path
// and its monitor sibling (tomcatXw.exe) so regen_service can copy both.
// Empty strings mean "neither is on disk" — the caller falls back to
// logging that fact to the caller instead of pretending to copy.
func resolvePrunsrvSource(bin string) (prunsrv, monitor string) {
	for _, base := range []string{"tomcat10", "tomcat9"} {
		if _, err := os.Stat(filepath.Join(bin, base+".exe")); err == nil {
			return filepath.Join(bin, base+".exe"), filepath.Join(bin, base+"w.exe")
		}
	}
	return "", ""
}

func (r *Registry) regenService(params json.RawMessage) (json.RawMessage, error) {
	bin := filepath.Join(r.cfg.TomcatHome, "bin")
	if !r.cfg.IsAllowedWorkDir(bin) {
		return nil, errors.New("regen_service: bin dir not allowed")
	}
	var (
		path, content  string
		serviceName    string
		prunsrvPath    string
		prunsrvCopied  bool
		prunsrvExisted bool
		prunsrvSrc     string
		monitorPath    string
		monitorCopied  bool
		monitorExisted bool
		monitorSrc     string
	)
	if runtime.GOOS == "windows" {
		serviceName = resolveServiceName(r.cfg.TomcatHome, r.cfg.ServiceName)
		path = filepath.Join(bin, "service.bat")
		content = buildWindowsService(r.cfg, serviceName)
		// Service install on Windows needs two renamed binaries in
		// <bin>: <ServiceName>.exe (prunsrv, used by "service.bat
		// install" //IS//<name>) and <ServiceName>w.exe (the GUI
		// monitor). Tomcat ships them as tomcat9/10 + the w suffix; the
		// operator used to rename them by hand. Helper writes the
		// copies when missing so "service.bat install" works without
		// going through the README dance.
		psrc, msrc := resolvePrunsrvSource(bin)
		prunsrvDst := filepath.Join(bin, serviceName+".exe")
		monitorDst := filepath.Join(bin, serviceName+"w.exe")
		prunsrvPath = prunsrvDst
		monitorPath = monitorDst
		prunsrvSrc = psrc
		monitorSrc = msrc
		if _, err := os.Stat(prunsrvDst); err == nil {
			prunsrvExisted = true
		} else if psrc != "" {
			if _, copyErr := copyFileForRegen(psrc, prunsrvDst); copyErr == nil {
				prunsrvCopied = true
			}
		}
		if _, err := os.Stat(monitorDst); err == nil {
			monitorExisted = true
		} else if msrc != "" {
			if _, copyErr := copyFileForRegen(msrc, monitorDst); copyErr == nil {
				monitorCopied = true
			}
		}
	} else {
		path = filepath.Join(bin, "service.sh")
		content = buildUnixService(r.cfg)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		return nil, fmt.Errorf("regen_service: write: %w", err)
	}
	return mustMarshal(RegenServiceData{
		Path:           path,
		Bytes:          len(content),
		ServiceName:    serviceName,
		PrunsrvPath:    prunsrvPath,
		PrunsrvCopied:  prunsrvCopied,
		PrunsrvExisted: prunsrvExisted,
		PrunsrvSource:  prunsrvSrc,
		MonitorPath:    monitorPath,
		MonitorCopied:  monitorCopied,
		MonitorExisted: monitorExisted,
		MonitorSource:  monitorSrc,
	}), nil
}

// copyFileForRegen is a plain byte copy used by regen_service to materialise
// the prunsrv / monitor copies. Private because regen_service is the only
// caller — the replace_jar path needs fsync + atomic-rename semantics
// (see copyFile in probe.go).
func copyFileForRegen(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	return io.Copy(out, in)
}

func buildUnixService(c *config.HelperConfig) string {
	return fmt.Sprintf(`#!/bin/sh
# Auto-generated by tomcat-ops-helper
export JAVA_HOME=%q
export CATALINA_HOME=%q
case "$1" in
  start)   %q start   ;;
  stop)    %q stop    ;;
  restart) %q stop; %q start ;;
  *) echo "usage: $0 {start|stop|restart}"; exit 1 ;;
esac
`,
		c.JDKHome,
		c.TomcatHome,
		filepath.Join(c.TomcatHome, "bin", "startup.sh"),
		filepath.Join(c.TomcatHome, "bin", "shutdown.sh"),
		filepath.Join(c.TomcatHome, "bin", "shutdown.sh"),
		filepath.Join(c.TomcatHome, "bin", "startup.sh"),
	)
}

// buildWindowsService writes a thin service.bat that maps
// "service.bat {start|stop|restart|install|remove}" onto the right
// prunsrv.exe call. For start/stop/restart we prefer the Windows
// service when it exists ("net start <name>"), and fall back to
// calling startup.bat / shutdown.bat directly otherwise — so a fresh
// Tomcat install that has never been turned into a service still
// behaves the way startup.bat does out of the box. JAVA_HOME and
// CATALINA_HOME are exported for every subcommand because prunsrv
// (used by install/remove) spawns the JVM via the service-control
// pipeline and needs both vars set before service creation; without
// them service.bat install fails with the misleading "CATALINA_HOME
// environment variable is not defined" message.
func buildWindowsService(c *config.HelperConfig, serviceName string) string {
	bin := filepath.Join(c.TomcatHome, "bin")
	startup := filepath.Join(bin, "startup.bat")
	shutdown := filepath.Join(bin, "shutdown.bat")
	prunsrv := filepath.Join(bin, serviceName+".exe")
	return fmt.Sprintf(`@echo off
rem Auto-generated by tomcat-ops-helper (service name: %s)
setlocal
set "JAVA_HOME=%s"
set "CATALINA_HOME=%s"
if /I "%%1"=="install" (
  if not exist "%s" (
    echo service.bat: %%1 needs tomcat9.exe/tomcat10.exe renamed first. Copy %%~dp0tomcat10.exe to %%~dp0%s.exe (Tomcat 10+) or tomcat9.exe to %%~dp0%s.exe (Tomcat 9). Run regen_service from the helper once Tomcat is installed under %%CATALINA_HOME%%\bin.
    exit /b 1
  )
  call "%s" //IS//%s --DisplayName="Apache Tomcat" ^
    --Install="%s" ^
    --JavaHome="%s" ^
    --CatalinaHome="%s" ^
    --Jvm="%s\jre\bin\server\jvm.dll" ^
    --StartMode=jvm --StopMode=jvm ^
    --StartClass=org.apache.catalina.startup.Bootstrap --StartParams=start ^
    --StopClass=org.apache.catalina.startup.Bootstrap --StopParams=stop ^
    --LogPath="%s"
  exit /b %%ERRORLEVEL%%
)
if /I "%%1"=="remove" (
  call "%s" //DS//%s
  exit /b %%ERRORLEVEL%%
)
if /I "%%1"=="start" (
  sc query %s >nul 2>&1
  if %%ERRORLEVEL%%==0 (
    net start %s
  ) else (
    call "%s"
  )
  exit /b %%ERRORLEVEL%%
)
if /I "%%1"=="stop" (
  sc query %s >nul 2>&1
  if %%ERRORLEVEL%%==0 (
    net stop %s
  ) else (
    call "%s"
  )
  exit /b %%ERRORLEVEL%%
)
if /I "%%1"=="restart" (
  sc query %s >nul 2>&1
  if %%ERRORLEVEL%%==0 (
    net stop %s
    net start %s
  ) else (
    call "%s"
    call "%s"
  )
  exit /b %%ERRORLEVEL%%
)
echo usage: %%~nx0 {install^|remove^|start^|stop^|restart}
exit /b 1
`,
		serviceName,
		c.JDKHome, c.TomcatHome,
		prunsrv,
		serviceName, serviceName,
		prunsrv, serviceName,
		prunsrv,
		c.JDKHome,
		c.TomcatHome,
		c.JDKHome,
		filepath.Join(c.LogDir),
		prunsrv, serviceName,
		serviceName, serviceName, startup,
		serviceName, serviceName, shutdown,
		serviceName, serviceName, serviceName, shutdown, startup,
	)
}

// WriteFileData is the JSON returned by "write_file".
type WriteFileData struct {
	Path        string `json:"path"`
	Bytes       int    `json:"bytes"`
	SHA256      string `json:"sha256"`
	Overwritten bool   `json:"overwritten"`
}

// ErrFileExists is returned when write_file would overwrite an existing file
// without overwrite=true. Exposed so callers (tests, server error mapping)
// can identify the failure mode.
var ErrFileExists = errors.New("file already exists")

// writeFile writes UTF-8 text to a file under allowed_dirs. The text is taken
// verbatim — no BOM is added. Writes go through `<path>.tmp` + fsync + rename
// so the destination either has the old content or the new content, never a
// partial write. By default existing files are left untouched; pass
// `overwrite=true` to replace them.
func (r *Registry) writeFile(params json.RawMessage) (json.RawMessage, error) {
	p, err := decodeParams(params)
	if err != nil {
		return nil, err
	}
	path, err := requireString(p, "path")
	if err != nil {
		return nil, fmt.Errorf("write_file: %w", err)
	}
	text, err := requireString(p, "text")
	if err != nil {
		return nil, fmt.Errorf("write_file: %w", err)
	}
	overwrite, _ := p["overwrite"].(bool)

	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("write_file: path: %w", err)
	}
	if !r.cfg.IsAllowedWorkDir(abs) {
		return nil, fmt.Errorf("write_file: path not in allowed dir: %s", abs)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return nil, fmt.Errorf("write_file: mkdir: %w", err)
	}

	existed := false
	if _, err := os.Stat(abs); err == nil {
		existed = true
		if !overwrite {
			return nil, fmt.Errorf("%w: %s", ErrFileExists, abs)
		}
	}

	payload := []byte(text) // UTF-8, no BOM, exact byte stream.
	tmp := abs + ".tmp"
	if err := writeAtomic(tmp, abs, payload, 0o644); err != nil {
		return nil, fmt.Errorf("write_file: %w", err)
	}

	sum, err := sha256Of(abs)
	if err != nil {
		return nil, fmt.Errorf("write_file: hash: %w", err)
	}
	return mustMarshal(WriteFileData{
		Path:        abs,
		Bytes:       len(payload),
		SHA256:      sum,
		Overwritten: overwrite && existed,
	}), nil
}

// appendTextFile appends UTF-8 text without a BOM and fsyncs it before
// returning. It is intended for small scripts, notes and configuration
// fragments where callers should not need base64 upload chunks.
func (r *Registry) appendTextFile(params json.RawMessage) (json.RawMessage, error) {
	p, err := decodeParams(params)
	if err != nil {
		return nil, fmt.Errorf("append_text_file: %w", err)
	}
	path, err := requireString(p, "path")
	if err != nil {
		return nil, fmt.Errorf("append_text_file: %w", err)
	}
	text, ok := p["text"].(string)
	if !ok {
		return nil, fmt.Errorf("append_text_file: %w: text", ErrMissingField)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("append_text_file: path: %w", err)
	}
	if !r.cfg.IsAllowedWorkDir(abs) {
		return nil, fmt.Errorf("append_text_file: path not in allowed dir: %s; allowed_dirs: %s", abs, strings.Join(r.cfg.AllowedDirs, ", "))
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return nil, fmt.Errorf("append_text_file: mkdir: %w", err)
	}
	f, err := os.OpenFile(abs, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("append_text_file: open: %w", err)
	}
	n, writeErr := f.WriteString(text)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return nil, fmt.Errorf("append_text_file: write: %w", writeErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("append_text_file: close: %w", closeErr)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("append_text_file: stat: %w", err)
	}
	return mustMarshal(map[string]any{"path": abs, "bytes_appended": n, "total_bytes": info.Size()}), nil
}

// writeAtomic writes payload to tmp, fsyncs, then renames tmp over dst. On
// any failure the tmp file is removed so we never leak a half-written file
// alongside an untouched dst.
func writeAtomic(tmp, dst string, payload []byte, mode os.FileMode) error {
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(payload); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ValidateData is the JSON returned by the "validate_config" action.
// It reports what helper *actually* loaded (paths, allowed_dirs, and
// the current health of every registered database connector) so the
// operator can confirm config.json edits took effect without scraping
// helper.log.
//
// Passwords are never returned — DBConfig.User is enough to verify
// the credential, and Status.LastError tells you why a connector is
// down without leaking anything.
type ValidateData struct {
	Now            time.Time                    `json:"now"`
	ConfigLoadedAt time.Time                    `json:"config_loaded_at"`
	TomcatHome     string                       `json:"tomcat_home"`
	TomcatExists   bool                         `json:"tomcat_exists"`
	JDKHome        string                       `json:"jdk_home"`
	JDKExists      bool                         `json:"jdk_exists"`
	LogDir         string                       `json:"log_dir"`
	LogDirExists   bool                         `json:"log_dir_exists"`
	AllowedDirs    []ValidateAllowedDir         `json:"allowed_dirs"`
	Connectors     map[string]ValidateConnector `json:"connectors"`
}

// ValidateAllowedDir is one entry of allowed_dirs with an existence check.
type ValidateAllowedDir struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
}

// ValidateConnector is the health snapshot of one DB connector.
type ValidateConnector struct {
	Configured    bool   `json:"configured"`
	Open          bool   `json:"open"`
	Driver        string `json:"driver,omitempty"`
	Host          string `json:"host,omitempty"`
	Port          int    `json:"port,omitempty"`
	Database      string `json:"database,omitempty"`
	CurrentSchema string `json:"current_schema,omitempty"`
	ReadOnly      bool   `json:"read_only,omitempty"` // forbid_cmd lock is active
	LatencyMs     int64  `json:"latency_ms,omitempty"`
	LastError     string `json:"last_error,omitempty"`
}

func (r *Registry) validateConfig(ctx context.Context) (json.RawMessage, error) {
	_, tomcatErr := os.Stat(r.cfg.TomcatHome)
	_, jdkErr := os.Stat(r.cfg.JDKHome)
	_, logErr := os.Stat(r.cfg.LogDir)

	dirs := make([]ValidateAllowedDir, 0, len(r.cfg.AllowedDirs))
	for _, d := range r.cfg.AllowedDirs {
		_, err := os.Stat(d)
		dirs = append(dirs, ValidateAllowedDir{Path: d, Exists: err == nil})
	}

	connectors := make(map[string]ValidateConnector, len(r.dbops))
	for name, op := range r.dbops {
		st := op.Status(ctx)
		connectors[name] = ValidateConnector{
			Configured:    st.Configured,
			Open:          st.Open,
			Driver:        st.Driver,
			Host:          st.Host,
			Port:          st.Port,
			Database:      st.Database,
			CurrentSchema: st.CurrentSchema,
			ReadOnly:      st.ReadOnly,
			LatencyMs:     st.LatencyMs,
			LastError:     st.LastError,
		}
	}

	data := ValidateData{
		Now:            time.Now().UTC(),
		ConfigLoadedAt: r.cfg.LoadedAt.UTC(),
		TomcatHome:     r.cfg.TomcatHome,
		TomcatExists:   tomcatErr == nil,
		JDKHome:        r.cfg.JDKHome,
		JDKExists:      jdkErr == nil,
		LogDir:         r.cfg.LogDir,
		LogDirExists:   logErr == nil,
		AllowedDirs:    dirs,
		Connectors:     connectors,
	}
	return mustMarshal(data), nil
}

// UploadGuideData is the JSON returned by the "upload_guide" action.
type UploadGuideData struct {
	Source      string   `json:"source"`
	Destination string   `json:"destination"`
	ServerURL   string   `json:"server_url"`
	ClientID    string   `json:"client_id,omitempty"`
	Steps       []string `json:"steps"`
}

// uploadGuide returns step-by-step instructions for uploading a file.
func (r *Registry) uploadGuide(params json.RawMessage) (json.RawMessage, error) {
	p, err := decodeParams(params)
	if err != nil {
		return nil, err
	}
	source, err := requireString(p, "source")
	if err != nil {
		return nil, fmt.Errorf("upload_guide: %w", err)
	}
	dest, err := requireString(p, "destination")
	if err != nil {
		return nil, fmt.Errorf("upload_guide: %w", err)
	}

	// Use the server_url from config to derive the upload endpoint.
	// Default to localhost:7777 if not available.
	serverURL := "http://127.0.0.1:7777"
	if r.cfg.ServerURL != "" {
		// Extract host:port from server_url (e.g., "wss://example.com:8443" -> "http://example.com:7777")
		serverURL = r.cfg.ServerURL
		// Convert wss:// to http://
		serverURL = strings.Replace(serverURL, "wss://", "http://", 1)
		serverURL = strings.Replace(serverURL, "ws://", "http://", 1)
		// Replace the port with 7777 (agent HTTP bridge)
		if idx := strings.LastIndex(serverURL, ":"); idx > 0 {
			serverURL = serverURL[:idx] + ":7777"
		}
	}

	clientID := r.cfg.ClientID

	// Build curl flags; include client_id when available.
	curlFlags := fmt.Sprintf("-F 'file=@%s' -F 'path=%s' -F 'sha256=$sha' -F 'overwrite=true'", source, dest)
	if clientID != "" {
		curlFlags += fmt.Sprintf(" -F 'client_id=%s'", clientID)
	}

	steps := []string{
		"1. Use bash tool to compute SHA256 hash:",
		fmt.Sprintf("   $sha = (Get-FileHash '%s' -Algorithm SHA256).Hash.ToLower()", source),
		"",
		"2. Use bash tool to upload via curl:",
		fmt.Sprintf("   curl.exe -k -X POST %s %s/upload", curlFlags, serverURL),
		"",
		"3. The upload endpoint returns JSON with 'upload_id', 'bytes', 'duration_ms', and 'result' (containing sha256, verified, overwritten).",
		"",
		"IMPORTANT: This tool only provides instructions. You must use bash tool to execute the commands above.",
	}

	data := UploadGuideData{
		Source:      source,
		Destination: dest,
		ServerURL:   serverURL,
		ClientID:    clientID,
		Steps:       steps,
	}
	return mustMarshal(data), nil
}
