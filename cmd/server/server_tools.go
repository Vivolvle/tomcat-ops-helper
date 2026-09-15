package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"tomcat-ops-helper/internal/protocol"
)

// reportProblemTool is the only MCP tool the server executes itself — every
// other tool is proxied to a connected helper. It intentionally works with
// zero helpers connected: agents should be able to file feedback even when
// the helper is offline.
const reportProblemTool = "report_problem"

// problemLogHeader seeds problem.md the first time an entry is appended.
const problemLogHeader = "# problem.md — 使用问题登记（MCP `report_problem` 自动写入）\n\n" +
	"其他项目的 agent 在使用 tomcat-helper 的 MCP 工具过程中遇到任何不方便、缺失能力或\n" +
	"报错，都应通过 `report_problem` 登记，而不是默默绕过。operator 会定期处理这里的内容。\n\n" +
	"<!-- 新条目由 server.exe 追加在文件末尾；上面的说明不要删。 -->\n\n"

// maxProblemFieldSize caps each free-text field so a runaway client cannot
// bloat problem.md in a single call.
const maxProblemFieldSize = 8 << 10

// errInvalidArguments marks validation failures so /mcp/call can answer with
// -32602 instead of the generic -32000 execution error.
var errInvalidArguments = errors.New("invalid arguments")

// serverToolDefs returns the tools the server itself owns. They are merged
// into /mcp/tools output under the top-level `server_tools` key.
func serverToolDefs() []protocol.ToolMeta {
	return []protocol.ToolMeta{
		{
			Name: reportProblemTool,
			Description: "Report a problem, gap, or inconvenience encountered while using the " +
				"tomcat-helper MCP tools. The entry is appended to problem.md on the operator " +
				"box and reviewed periodically. Use this instead of silently working around a " +
				"missing or awkward capability. Works even when no helper is connected.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "description": {"type": "string", "description": "What went wrong or was inconvenient. Include the tool/command and the error or observation."},
    "tool": {"type": "string", "description": "Name of the helper tool involved, e.g. exec, read_tail, upload_file (optional)."},
    "expected": {"type": "string", "description": "What you expected to happen instead (optional)."},
    "source": {"type": "string", "description": "Who is reporting, e.g. project or agent name (optional)."}
  },
  "required": ["description"]
}`),
		},
	}
}

// isServerTool reports whether name is executed locally by the server.
func isServerTool(name string) bool {
	for _, t := range serverToolDefs() {
		if t.Name == name {
			return true
		}
	}
	return false
}

// reportProblemArgs is the parsed report_problem input.
type reportProblemArgs struct {
	Description string `json:"description"`
	Tool        string `json:"tool"`
	Expected    string `json:"expected"`
	Source      string `json:"source"`
}

// problemEntry renders one markdown block for problem.md.
func problemEntry(a reportProblemArgs, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## [%s]\n", now.Format("2006-01-02 15:04:05"))
	if a.Source != "" {
		fmt.Fprintf(&b, "- 来源: %s\n", a.Source)
	}
	if a.Tool != "" {
		fmt.Fprintf(&b, "- 工具: %s\n", a.Tool)
	}
	fmt.Fprintf(&b, "- 问题: %s\n", a.Description)
	if a.Expected != "" {
		fmt.Fprintf(&b, "- 期望: %s\n", a.Expected)
	}
	b.WriteString("\n")
	return b.String()
}

// appendProblemEntry appends block to path, seeding the header when the file
// does not exist yet. It returns the number of bytes written for the entry
// itself (excluding the header).
func appendProblemEntry(path, block string) (int, error) {
	created := false
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		created = true
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	payload := block
	if created {
		payload = problemLogHeader + block
	}
	if _, err := f.WriteString(payload); err != nil {
		return 0, err
	}
	return len(block), nil
}

// executeServerTool dispatches a server-local tool call. text is the MCP
// content string; data is the machine-readable payload.
func (r *relay) executeServerTool(name string, args json.RawMessage) (data json.RawMessage, text string, err error) {
	switch name {
	case reportProblemTool:
		r.problemMu.Lock()
		defer r.problemMu.Unlock()
		return r.executeReportProblem(args)
	default:
		return nil, "", fmt.Errorf("unknown server tool %q", name)
	}
}

// executeReportProblem validates the arguments and appends one entry to the
// configured problem log. Callers must hold r.problemMu.
func (r *relay) executeReportProblem(args json.RawMessage) (json.RawMessage, string, error) {
	var a reportProblemArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, "", fmt.Errorf("%w: %v", errInvalidArguments, err)
		}
	}
	a.Description = strings.TrimSpace(a.Description)
	a.Tool = strings.TrimSpace(a.Tool)
	a.Expected = strings.TrimSpace(a.Expected)
	a.Source = strings.TrimSpace(a.Source)
	if a.Description == "" {
		return nil, "", fmt.Errorf("%w: description is required", errInvalidArguments)
	}
	for field, v := range map[string]string{
		"description": a.Description,
		"tool":        a.Tool,
		"expected":    a.Expected,
		"source":      a.Source,
	} {
		if len(v) > maxProblemFieldSize {
			return nil, "", fmt.Errorf("%w: %s exceeds %d bytes", errInvalidArguments, field, maxProblemFieldSize)
		}
	}

	path := r.cfg.ProblemLog
	if path == "" {
		path = "problem.md"
	}
	written, err := appendProblemEntry(path, problemEntry(a, time.Now()))
	if err != nil {
		return nil, "", fmt.Errorf("write problem log: %w", err)
	}
	out, err := json.Marshal(map[string]any{
		"ok":    true,
		"path":  path,
		"bytes": written,
	})
	if err != nil {
		return nil, "", err
	}
	return out, fmt.Sprintf("problem recorded to %s (%d bytes appended)", path, written), nil
}
