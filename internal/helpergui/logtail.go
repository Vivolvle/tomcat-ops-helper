package helpergui

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
)

// TailLine is one entry of the helper.log (audit JSONL) tail.
type TailLine struct {
	TS       string `json:"ts"`
	Cmd      string `json:"cmd,omitempty"`
	ExitCode any    `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
	Raw      string `json:"raw,omitempty"` // original line when it is not valid JSON
}

// TailFile reads forward from byte offset, returning at most maxLines
// parsed lines plus the new offset. offset<0 (or 0 with the
// startAtEnd flag handled by the caller) means "start near the end".
// The helper only appends (O_APPEND), so a byte offset is a stable
// cursor. Reads use a shared-open so the tail works while helper holds
// the file open for writing on Windows.
func TailFile(path string, offset int64, maxLines int) (lines []TailLine, newOffset int64, err error) {
	if maxLines <= 0 {
		maxLines = 200
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, 0, err
	}
	if offset < 0 || offset > st.Size() {
		offset = st.Size() - 64*1024
		if offset < 0 {
			offset = 0
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, offset, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, 0); err != nil {
		return nil, offset, err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		text := sc.Text()
		offset += int64(len(text)) + 1 // + newline (CRLF tails count the \r inside text)
		if strings.TrimSpace(text) == "" {
			continue
		}
		lines = append(lines, parseTailLine(text))
		if len(lines) >= maxLines {
			break
		}
	}
	if err := sc.Err(); err != nil && len(lines) < maxLines {
		lines = append(lines, TailLine{Raw: "读取日志中断: " + err.Error()})
	}
	return lines, offset, nil
}

// parseTailLine decodes one helper.log JSONL entry (execshell.AuditEntry
// shape); non-JSON lines survive verbatim in Raw.
func parseTailLine(text string) TailLine {
	var entry struct {
		Timestamp string `json:"timestamp"`
		Cmd       string `json:"cmd"`
		WorkDir   string `json:"work_dir"`
		ExitCode  any    `json:"exit_code"`
		Error     string `json:"error"`
	}
	if err := json.Unmarshal([]byte(text), &entry); err != nil || entry.Cmd == "" {
		return TailLine{Raw: text}
	}
	return TailLine{TS: entry.Timestamp, Cmd: entry.Cmd, ExitCode: entry.ExitCode, Error: entry.Error}
}
