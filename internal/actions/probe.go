package actions

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// OSProbe is the production Probe. It shells out to platform utilities and
// parses their output. The helpers here are best-effort: missing tools degrade
// gracefully rather than fail the action.
type OSProbe struct{}

// NewOSProbe constructs an OSProbe.
func NewOSProbe() *OSProbe { return &OSProbe{} }

// JavaProcesses collects JVMs the helper considers interesting. We look for
// processes whose command line mentions tomcat_home or contains "tomcat".
func (p *OSProbe) JavaProcesses(ctx context.Context) ([]JavaProcess, error) {
	if runtime.GOOS == "windows" {
		return p.javaProcessesWindows(ctx)
	}
	return p.javaProcessesUnix(ctx)
}

func (p *OSProbe) javaProcessesWindows(ctx context.Context) ([]JavaProcess, error) {
	// Enumerate ALL processes and filter in Go with looksLikeJVM. A
	// server-side `Name='java.exe'` filter misses Tomcat installed as a
	// Windows service: procrun's default --StartMode=jvm loads jvm.dll
	// into the renamed service binary (e.g. tomcat10-esb.exe), so no
	// java.exe process ever exists and status/wait_for_catalina saw a
	// permanent empty list (2026-09-25 report).
	//
	// wmic was deprecated in newer Windows builds and is often absent from
	// PATH. Fall back to PowerShell so we keep working on current OS images.
	if _, err := exec.LookPath("wmic"); err == nil {
		cmd := exec.CommandContext(ctx, "wmic", "process", "get", "ProcessId,Name,CommandLine", "/format:list")
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err == nil {
			return filterInterestingProcesses(parseWmicList(out.String())), nil
		}
	}
	psCmd := `Get-CimInstance Win32_Process | Select-Object ProcessId,Name,CommandLine | ConvertTo-Csv -NoTypeInformation`
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", psCmd)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("powershell: %w", err)
	}
	return filterInterestingProcesses(parsePowerShellCSV(out.String())), nil
}

// looksLikeJVM reports whether an enumerated Windows process could be
// hosting the Tomcat JVM: a java executable, or — for Tomcat registered as
// a Windows service via procrun with StartMode=jvm — the renamed prunsrv
// binary whose image name or command line references tomcat/catalina
// (e.g. "D:\...\tomcat10-esb.exe" //RS//tomcat10-esb). The caller still
// applies isInteresting on top of this for status/kill filtering.
func looksLikeJVM(p JavaProcess) bool {
	name := strings.ToLower(p.Name)
	if strings.HasPrefix(name, "java") { // java.exe, javaw.exe
		return true
	}
	if strings.Contains(name, "tomcat") || strings.Contains(name, "catalina") {
		return true
	}
	lc := strings.ToLower(p.Cmdline)
	return strings.Contains(lc, "tomcat") || strings.Contains(lc, "catalina")
}

func filterInterestingProcesses(procs []JavaProcess) []JavaProcess {
	var out []JavaProcess
	for _, p := range procs {
		if p.PID == 0 && p.Cmdline == "" {
			continue
		}
		if looksLikeJVM(p) {
			out = append(out, p)
		}
	}
	return out
}

func parsePowerShellCSV(s string) []JavaProcess {
	var procs []JavaProcess
	scanner := bufio.NewScanner(strings.NewReader(s))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	header := map[string]int{}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		fields := splitCSV(line)
		if len(fields) == 0 {
			continue
		}
		if _, ok := header["ProcessId"]; !ok {
			for i, f := range fields {
				// Trim BOTH quotes: TrimPrefix alone left a trailing quote
				// on the key, so header["ProcessId"] never matched and the
				// parser silently returned an empty list for every input.
				header[strings.TrimSpace(strings.Trim(f, "\""))] = i
			}
			continue
		}
		pidIdx, pidOK := header["ProcessId"]
		cmdIdx, cmdOK := header["CommandLine"]
		if !pidOK || !cmdOK {
			continue
		}
		pid, err := strconv.Atoi(strings.Trim(strings.TrimSpace(fields[pidIdx]), "\""))
		if err != nil {
			continue
		}
		jp := JavaProcess{
			PID:     pid,
			Cmdline: strings.Trim(strings.TrimSpace(fields[cmdIdx]), "\""),
		}
		if nameIdx, ok := header["Name"]; ok {
			jp.Name = strings.Trim(strings.TrimSpace(fields[nameIdx]), "\"")
		}
		procs = append(procs, jp)
	}
	return procs
}

// splitCSV splits a single line of CSV honouring quoted fields. It is good
// enough for ConvertTo-Csv output (no escaped quotes expected).
func splitCSV(line string) []string {
	var out []string
	var cur strings.Builder
	inQuotes := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '"':
			inQuotes = !inQuotes
			cur.WriteByte(c)
		case c == ',' && !inQuotes:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	out = append(out, cur.String())
	return out
}

func (p *OSProbe) javaProcessesUnix(ctx context.Context) ([]JavaProcess, error) {
	cmd := exec.CommandContext(ctx, "ps", "-eo", "pid,user,args")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	var procs []JavaProcess
	scanner := bufio.NewScanner(&out)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		cmdline := strings.Join(fields[2:], " ")
		if !strings.Contains(strings.ToLower(cmdline), "java") {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		procs = append(procs, JavaProcess{PID: pid, User: fields[1], Cmdline: cmdline})
	}
	return procs, nil
}

func parseWmicList(s string) []JavaProcess {
	var procs []JavaProcess
	var cur JavaProcess
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			if cur.PID != 0 || cur.Cmdline != "" {
				procs = append(procs, cur)
			}
			cur = JavaProcess{}
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := line[:eq]
		val := line[eq+1:]
		switch key {
		case "ProcessId":
			if pid, err := strconv.Atoi(strings.TrimSpace(val)); err == nil {
				cur.PID = pid
			}
		case "Name":
			cur.Name = val
		case "CommandLine":
			cur.Cmdline = val
		}
	}
	if cur.PID != 0 || cur.Cmdline != "" {
		procs = append(procs, cur)
	}
	return procs
}

// ListeningPorts returns TCP listeners. On Windows we use netstat -ano; on
// Unix we use ss -ltnp (fallback to netstat).
func (p *OSProbe) ListeningPorts(ctx context.Context) ([]Port, error) {
	if runtime.GOOS == "windows" {
		cmd := exec.CommandContext(ctx, "netstat", "-ano", "-p", "TCP")
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("netstat: %w", err)
		}
		return parseWindowsNetstat(out.String()), nil
	}
	var cmd *exec.Cmd
	if _, err := exec.LookPath("ss"); err == nil {
		cmd = exec.CommandContext(ctx, "ss", "-ltnp")
	} else {
		cmd = exec.CommandContext(ctx, "netstat", "-ltnp")
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ss/netstat: %w", err)
	}
	return parseUnixNetstat(out.String()), nil
}

func parseWindowsNetstat(s string) []Port {
	var out []Port
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "TCP") && !strings.HasPrefix(line, "UDP") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		proto := fields[0]
		addr := fields[1]
		var pid int
		if len(fields) >= 5 {
			if p, err := strconv.Atoi(fields[4]); err == nil {
				pid = p
			}
		}
		if !strings.HasPrefix(proto, "TCP") || !strings.Contains(addr, ":") {
			continue
		}
		if !strings.HasSuffix(addr, ":0") {
			out = append(out, Port{Proto: "tcp", Addr: addr, PID: pid})
		}
	}
	return out
}

func parseUnixNetstat(s string) []Port {
	var out []Port
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "LISTEN") && !strings.HasPrefix(line, "tcp") && !strings.HasPrefix(line, "udp") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		addr := fields[3]
		if !strings.Contains(addr, ":") {
			continue
		}
		out = append(out, Port{Proto: "tcp", Addr: addr})
	}
	return out
}

// readFileTail returns up to lines of a file's tail.
func readFileTail(path string, lines int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if lines <= 0 {
		return nil, nil
	}
	ring := make([]string, 0, lines)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		if len(ring) < lines {
			ring = append(ring, scanner.Text())
		} else {
			ring = append(ring[1:], scanner.Text())
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return ring, nil
}

// copyFile is a tiny io.Copy wrapper that also fsyncs for durability.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// sha256Of returns the lowercase hex SHA-256 of path. Used by replace_jar
// to confirm the file landed intact.
func sha256Of(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := newHash()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hexOf(h), nil
}
