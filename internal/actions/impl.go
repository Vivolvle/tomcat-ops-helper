package actions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"tomcat-ops-helper/internal/config"
	"tomcat-ops-helper/internal/execshell"
)

// StatusData is the JSON returned by the "status" action.
type StatusData struct {
	Hostname    string        `json:"hostname"`
	OS          string        `json:"os"`
	Now         time.Time     `json:"now"`
	TomcatHome  string        `json:"tomcat_home"`
	TomcatExists bool         `json:"tomcat_exists"`
	JavaProcs   []JavaProcess `json:"java_processes"`
	Ports       []Port        `json:"listening_ports"`
}

func (r *Registry) status(ctx context.Context) (json.RawMessage, error) {
	procs, err := r.probe.JavaProcesses(ctx)
	if err != nil {
		return nil, fmt.Errorf("status: java processes: %w", err)
	}
	filtered := procs[:0]
	for _, p := range procs {
		if isInteresting(p, r.cfg.TomcatHome) {
			filtered = append(filtered, p)
		}
	}
	ports, err := r.probe.ListeningPorts(ctx)
	if err != nil {
		return nil, fmt.Errorf("status: ports: %w", err)
	}
	host, _ := os.Hostname()
	_, statErr := os.Stat(r.cfg.TomcatHome)
	data := StatusData{
		Hostname:     host,
		OS:           runtime.GOOS,
		Now:          time.Now().UTC(),
		TomcatHome:   r.cfg.TomcatHome,
		TomcatExists: statErr == nil,
		JavaProcs:    filtered,
		Ports:        ports,
	}
	return mustMarshal(data), nil
}

func isInteresting(p JavaProcess, tomcatHome string) bool {
	lc := strings.ToLower(p.Cmdline)
	if strings.Contains(lc, "tomcat") {
		return true
	}
	if strings.Contains(lc, "catalina") {
		return true
	}
	if tomcatHome != "" && strings.Contains(lc, strings.ToLower(filepath.ToSlash(tomcatHome))) {
		return true
	}
	return false
}

// LogFile describes one entry returned by list_logs.
type LogFile struct {
	Name    string    `json:"name"`
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
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, LogFile{Name: e.Name(), Size: info.Size(), ModTime: info.ModTime().UTC()})
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

// RestartData is the JSON returned by "restart".
type RestartData struct {
	Shutdown execshell.Result `json:"shutdown"`
	Startup  execshell.Result `json:"startup"`
}

func (r *Registry) restart(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	p, err := decodeParams(params)
	if err != nil {
		return nil, err
	}
	timeout := r.cfg.CommandTimeout
	if v, ok := p["timeout"].(float64); ok && v > 0 {
		timeout = time.Duration(v * float64(time.Second))
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

	downCtx, cancel := context.WithTimeout(ctx, timeout)
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
	upCtx, upCancel := context.WithTimeout(ctx, timeout)
	defer upCancel()
	startup, _ := r.executor.Exec(upCtx, binQuote(up, runtime.GOOS), workDir)

	return mustMarshal(RestartData{Shutdown: shutdown, Startup: startup}), nil
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
	Source     string `json:"source"`
	Dest       string `json:"dest"`
	SHA256     string `json:"sha256"`
	Bytes      int64  `json:"bytes"`
	Restarted  bool   `json:"restarted,omitempty"`
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
		raw, _ := r.restart(ctx, mustMarshal(map[string]any{"timeout": float64(r.cfg.CommandTimeout.Seconds())}))
		data.Restarted = true
		// Discard restart detail from response; the action that called us gets
		// the raw restart payload already.
		_ = raw
	}
	return mustMarshal(data), nil
}

// KillZombieData is the JSON returned by "kill_zombie".
type KillZombieData struct {
	Killed []int `json:"killed"`
	Found  []int `json:"found"`
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
		if !isInteresting(jp, r.cfg.TomcatHome) {
			continue
		}
		match := hasPID && int(pidF) == jp.PID
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

// RegenServiceData is the JSON returned by "regen_service".
type RegenServiceData struct {
	Path    string `json:"path"`
	Bytes   int    `json:"bytes"`
	Content string `json:"content,omitempty"`
}

func (r *Registry) regenService(params json.RawMessage) (json.RawMessage, error) {
	bin := filepath.Join(r.cfg.TomcatHome, "bin")
	if !r.cfg.IsAllowedWorkDir(bin) {
		return nil, errors.New("regen_service: bin dir not allowed")
	}
	var path, content string
	if runtime.GOOS == "windows" {
		path = filepath.Join(bin, "service.bat")
		content = buildWindowsService(r.cfg)
	} else {
		path = filepath.Join(bin, "service.sh")
		content = buildUnixService(r.cfg)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		return nil, fmt.Errorf("regen_service: write: %w", err)
	}
	return mustMarshal(RegenServiceData{Path: path, Bytes: len(content)}), nil
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
  *) echo "usage: $0 {{start|stop|restart}}"; exit 1 ;;
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

func buildWindowsService(c *config.HelperConfig) string {
	return fmt.Sprintf(`@echo off
rem Auto-generated by tomcat-ops-helper
set JAVA_HOME=%s
set CATALINA_HOME=%s
if "%%1"=="start" call "%s"
if "%%1"=="stop" call "%s"
if "%%1"=="restart" call "%s" && call "%s"
`,
		c.JDKHome, c.TomcatHome,
		filepath.Join(c.TomcatHome, "bin", "startup.bat"),
		filepath.Join(c.TomcatHome, "bin", "shutdown.bat"),
		filepath.Join(c.TomcatHome, "bin", "shutdown.bat"),
		filepath.Join(c.TomcatHome, "bin", "startup.bat"),
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
