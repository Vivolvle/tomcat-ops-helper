package helpergui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLog(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "helper.log")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestLogTailOffsetCursors: offset 0 starts near the end; appending and
// reusing the cursor returns only the new line.
func TestLogTailOffsetCursors(t *testing.T) {
	p := writeLog(t,
		`{"timestamp":"2026-10-01T10:00:00Z","cmd":"status","exit_code":0}`,
		`{"timestamp":"2026-10-01T10:00:01Z","cmd":"list_logs","exit_code":0}`,
	)
	lines, off, err := TailFile(p, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || lines[0].Cmd != "status" {
		t.Fatalf("initial tail wrong: %+v", lines)
	}

	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString(`{"timestamp":"2026-10-01T10:00:02Z","cmd":"tail","exit_code":0,"error":"boom"}` + "\n")
	_ = f.Close()

	lines, off2, err := TailFile(p, off, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0].Cmd != "tail" || lines[0].Error != "boom" {
		t.Fatalf("incremental tail wrong: %+v", lines)
	}
	if off2 <= off {
		t.Fatalf("offset must advance: %d → %d", off, off2)
	}
}

// TestLogTailRawFallback: non-JSON lines survive verbatim in Raw.
func TestLogTailRawFallback(t *testing.T) {
	p := writeLog(t, "not-json-at-all")
	lines, _, err := TailFile(p, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0].Raw != "not-json-at-all" {
		t.Fatalf("raw fallback wrong: %+v", lines)
	}
}

// TestLogTailMissingFile: a missing file is an error the route reports.
func TestLogTailMissingFile(t *testing.T) {
	if _, _, err := TailFile(filepath.Join(t.TempDir(), "nope.log"), 0, 10); err == nil {
		t.Fatal("expected error for missing file")
	}
}
