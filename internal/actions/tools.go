package actions

import "encoding/json"

// Tool describes an action's MCP-style metadata: its name, what it does, and
// the JSON Schema describing its input parameters. The Server exposes this
// list to MCP clients; callers fill in `arguments` matching `InputSchema`.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Tools returns the metadata for every action supported by this helper.
//
// The core list is static; database tools (esb_db_operator /
// erp_db_operator) are appended only when a connector has been wired
// in via WithDB. Adding a new action requires adding a matching entry
// here AND a case in Registry.Dispatch. Keep schemas small and avoid
// oneOf/anyOf unless really needed; most LLM clients struggle with
// anything beyond flat objects.
func (r *Registry) Tools() []Tool {
	core := coreTools()
	out := make([]Tool, 0, len(core)+len(r.dbops))
	out = append(out, core...)
	for _, def := range dbOpToolDefs {
		if _, ok := r.dbops[def.Name]; ok {
			out = append(out, def)
		}
	}
	return out
}

// coreTools is the static, always-present tool list.
func coreTools() []Tool {
	return []Tool{
		{Name: "list_exec", Description: "List currently running helper shell commands with PID, arguments, working directory and start time. Use kill_pid to stop a command.", InputSchema: emptyObjectSchema},
		{Name: "read_file", Description: "Read an existing UTF-8 text file in allowed_dirs, up to 1 MiB. Returns exact text and SHA256. Local editor tools cannot read remote helper files.", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`)},
		{Name: "replace_text", Description: "Atomically replace literal text in an existing UTF-8 file in allowed_dirs, up to 1 MiB. Preserves BOM and line endings. Default expected_count=1; mismatch leaves file unchanged. Optional expected_sha256 checks the version read earlier.", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"old_text":{"type":"string","minLength":1},"new_text":{"type":"string"},"expected_count":{"type":"integer","minimum":1},"expected_sha256":{"type":"string"}},"required":["path","old_text","new_text"],"additionalProperties":false}`)},
		{
			Name:        "status",
			Description: "Return hostname, OS, Tomcat home existence, JVM processes (filtered to Tomcat/catalina), and listening TCP ports.",
			InputSchema: emptyObjectSchema,
		},
		{
			Name:        "list_logs",
			Description: "List files in log_dir (or a directory under allowed_dirs) with name, size and modification time, newest first.",
			InputSchema: listLogsSchema,
		},
		{
			Name:        "tail",
			Description: "Read the last N lines of a file inside allowed_dirs. N defaults to 200, max 5000.",
			InputSchema: tailSchema,
		},
		{
			Name:        "stop_service",
			Description: "Stop the Tomcat Windows service via sc stop. Waits for catalina processes to confirm exit. Returns the service name, sc stop result, and whether catalina confirmed exited. Use this to stop Tomcat without immediately starting it — for deploy or manual control.",
			InputSchema: stopServiceSchema,
		},
		{
			Name:        "start_service",
			Description: "Start the Tomcat Windows service via sc start. Waits for service to reach RUNNING state. Returns the service name, sc start result, and whether the service confirmed RUNNING.",
			InputSchema: startServiceSchema,
		},
		{
			Name:        "restart",
			Description: "Restart Tomcat. On Windows uses sc stop/sc start with separate stop_timeout and start_timeout (default cfg.command_timeout each). Gives you separate stop/start control so long startup (deployWAR etc.) doesn't cause a single timeout. Optionally accepts stop_timeout and start_timeout params.",
			InputSchema: restartSchema,
		},
		{
			Name:        "deploy",
			Description: "Full deploy cycle: stop_service → wait for catalina exit → replace_jar → start_service. Use this one-shot action instead of manually calling stop/replace/start. Requires src and dest for the jar, plus optional stop_timeout/start_timeout.",
			InputSchema: deploySchema,
		},
		{
			Name:        "wait_for_catalina",
			Description: "Poll until catalina processes reach a target state: 'gone' (all catalina JVMs exited) or 'running' (at least one catalina JVM found). Returns whether the state was reached and if a timeout occurred.",
			InputSchema: waitCatalinaSchema,
		},
		{
			Name:        "replace_jar",
			Description: "Copy src over dest (both must be under allowed_dirs), backing up the original as dest.bak. Optionally restart Tomcat.",
			InputSchema: replaceJarSchema,
		},
		{
			Name:        "kill_zombie",
			Description: "Find Tomcat/catalina JVMs matching pid or pattern, kill -9 / taskkill /F. Returns the candidate set and per-PID kill results. When a specific pid is given, it will also match catalina processes even if isInteresting does not catch them — the PID is authoritative.",
			InputSchema: killZombieSchema,
		},
		{
			Name:        "kill_pid",
			Description: "Terminate any process by exact PID. On Windows, tree defaults true and uses taskkill /T; force defaults true. Refuses to terminate the helper process itself. Use this for non-Tomcat processes such as a foreground redis-server left behind by exec.",
			InputSchema: killPIDSchema,
		},
		{
			Name:        "regen_service",
			Description: "Regenerate service.sh (Unix) or service.bat (Windows) under tomcat_home/bin from cfg.",
			InputSchema: emptyObjectSchema,
		},
		{
			Name:        "exec",
			Description: "Run a single shell command with blacklist and allowed_dirs enforcement, output capped at cfg.max_output_size. Commands may run concurrently. On Windows a timeout terminates the complete child process tree and releases inherited output handles. Default shell is cmd.exe /C on Windows or sh -c on Unix; double-quoted arguments survive verbatim (paths with spaces are safe). For PowerShell cmdlets pass \"shell\":\"powershell\" to run the command as an encoded PowerShell script. For an intentional service/background process, use its service manager or a detached launcher; use kill_pid for a leftover process. Shell redirect/pipes (2>&1, |, &, ;) are NOT supported by exec; for multi-step scripts write a .cmd/.ps1 file via write_file and run that. Use sparingly; prefer named actions.",
			InputSchema: execSchema,
		},
		{
			Name:        "write_file",
			Description: "Write UTF-8 text to a file under allowed_dirs. No BOM. Atomic via tmp+rename. Refuses to overwrite an existing file unless overwrite=true. Returns bytes_written and sha256.",
			InputSchema: writeFileSchema,
		},
		{
			Name:        "append_text_file",
			Description: "Append UTF-8 text to a small file under allowed_dirs. Creates parent directories and the file when missing, writes no BOM, fsyncs before returning, and reports appended and total bytes.",
			InputSchema: appendTextFileSchema,
		},
		{
			Name:        "validate_config",
			Description: "Return what helper actually loaded: tomcat_home / jdk_home / log_dir existence, every allowed_dirs entry with existence check, and a per-connector health snapshot (Open / Driver / Host / Port / Database / LatencyMs / LastError). Connectors that were never wired in (esb / erp blocks missing from config.json) are omitted. Passwords are never returned. Use after editing config.json and restarting helper to confirm changes took effect.",
			InputSchema: emptyObjectSchema,
		},
		{
			Name:        "upload_guide",
			Description: "Return step-by-step instructions for uploading a file from the operator machine to the helper. This is a documentation tool - it does NOT perform the upload, it tells the agent HOW to do it via bash + curl. Use this when you need to upload a file but don't know the upload procedure.",
			InputSchema: uploadGuideSchema,
		},
	}
}

// JSON Schemas below are kept as raw JSON literals so they round-trip cleanly
// over the wire and stay human-reviewable. They target a strict-but-friendly
// subset that works with both Claude tool-use and MCP clients.

var emptyObjectSchema = json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)

var listLogsSchema = json.RawMessage(`{
	"type":"object",
	"properties":{
		"dir":{"type":"string","description":"Directory to list; defaults to log_dir and must be under allowed_dirs."}
	},
	"additionalProperties":false
}`)

var tailSchema = json.RawMessage(`{
	"type":"object",
	"properties":{
		"file":{"type":"string","description":"Absolute path; directory must be under allowed_dirs."},
		"lines":{"type":"integer","description":"Lines from end; default 200, max 5000.","minimum":1,"maximum":5000}
	},
	"required":["file"],
	"additionalProperties":false
}`)

var restartSchema = json.RawMessage(`{
	"type":"object",
	"properties":{
		"stop_timeout":{"type":"integer","description":"Timeout in seconds for the stop step; defaults to cfg.command_timeout.","minimum":1},
		"start_timeout":{"type":"integer","description":"Timeout in seconds for the start step; defaults to cfg.command_timeout (same as stop_timeout if not set).","minimum":1}
	},
	"additionalProperties":false
}`)

var stopServiceSchema = json.RawMessage(`{
	"type":"object",
	"properties":{
		"timeout":{"type":"integer","description":"Timeout in seconds for sc stop; defaults to cfg.command_timeout.","minimum":1},
		"wait_timeout":{"type":"integer","description":"Timeout in seconds to wait for catalina processes to exit after sc stop; defaults to timeout.","minimum":1}
	},
	"additionalProperties":false
}`)

var startServiceSchema = json.RawMessage(`{
	"type":"object",
	"properties":{
		"timeout":{"type":"integer","description":"Timeout in seconds for sc start and wait for RUNNING state; defaults to cfg.command_timeout.","minimum":1}
	},
	"additionalProperties":false
}`)

var waitCatalinaSchema = json.RawMessage(`{
	"type":"object",
	"properties":{
		"state":{"type":"string","description":"Target state: 'gone' waits for catalina processes to exit; 'running' waits for at least one catalina process to appear. Defaults to 'gone'.","enum":["gone","running"]},
		"timeout":{"type":"integer","description":"Maximum seconds to poll; defaults to cfg.command_timeout.","minimum":1}
	},
	"additionalProperties":false
}`)

var deploySchema = json.RawMessage(`{
	"type":"object",
	"properties":{
		"src":{"type":"string","description":"Source jar path; must be under allowed_dirs."},
		"dest":{"type":"string","description":"Destination jar path; must be under allowed_dirs."},
		"restart":{"type":"boolean","description":"If true (default), also stop and start the service around the jar replacement."},
		"stop_timeout":{"type":"integer","description":"Timeout in seconds for stop_service step.","minimum":1},
		"start_timeout":{"type":"integer","description":"Timeout in seconds for start_service step.","minimum":1}
	},
	"required":["src","dest"],
	"additionalProperties":false
}`)

var replaceJarSchema = json.RawMessage(`{
	"type":"object",
	"properties":{
		"src":{"type":"string","description":"Source jar path; must be under allowed_dirs."},
		"dest":{"type":"string","description":"Destination jar path; must be under allowed_dirs."},
		"restart":{"type":"boolean","description":"If true, restart Tomcat after copying."}
	},
	"required":["src","dest"],
	"additionalProperties":false
}`)

var killZombieSchema = json.RawMessage(`{
	"type":"object",
	"properties":{
		"pid":{"type":"integer","description":"Exact PID to kill. Matches catalina processes even if isInteresting does not catch them — the PID is authoritative.","minimum":1},
		"pattern":{"type":"string","description":"Substring to match in cmdline (case-insensitive)."}
	},
	"additionalProperties":false
}`)

var killPIDSchema = json.RawMessage(`{
	"type":"object",
	"properties":{
		"pid":{"type":"integer","description":"Exact process ID to terminate.","minimum":1},
		"force":{"type":"boolean","description":"Force termination. Defaults true."},
		"tree":{"type":"boolean","description":"Windows: also terminate child processes. Defaults true; ignored on other platforms."}
	},
	"required":["pid"],
	"additionalProperties":false
}`)

var execSchema = json.RawMessage(`{
	"type":"object",
	"properties":{
		"cmd":{"type":"string","description":"Shell command. Runs through cmd.exe /C on Windows or sh -c on Unix."},
		"work_dir":{"type":"string","description":"Working directory; must be under allowed_dirs. Defaults to cfg.tomcat_home; if that is missing or not allowed, the first existing allowed_dirs entry is used and reported back as work_dir with work_dir_fallback=true."},
		"shell":{"type":"string","enum":["powershell","pwsh","cmd","sh"],"description":"Optional shell override. 'powershell' or 'pwsh' wraps the command in powershell -NoProfile -NonInteractive -Command with a Set-Location prologue so paths with spaces and PowerShell-specific syntax work without the cmd /C tokenize quirks. Default is cmd /C on Windows, sh -c on Unix."},
		"timeout":{"type":"integer","description":"Per-command timeout in seconds. Defaults to cfg.command_timeout (30s). On Windows, expiry terminates the full spawned process tree. Increase for slow commands like net stop or large file extraction.","minimum":1}
	},
	"required":["cmd"],
	"additionalProperties":false
}`)

var writeFileSchema = json.RawMessage(`{
	"type":"object",
	"properties":{
		"path":{"type":"string","description":"Absolute destination path; the file's parent dir must be under allowed_dirs."},
		"text":{"type":"string","description":"UTF-8 content. No BOM is added; round-trip is exact."},
		"overwrite":{"type":"boolean","description":"Allow replacing an existing file. Default false."}
	},
	"required":["path","text"],
	"additionalProperties":false
}`)

var appendTextFileSchema = json.RawMessage(`{
	"type": "object",
	"properties":{
		"path":{"type":"string","description":"Absolute destination path; its parent must be under allowed_dirs."},
		"text":{"type":"string","description":"UTF-8 text appended exactly; no BOM or newline is added automatically."}
	},
	"required":["path","text"],
	"additionalProperties":false
}`)

var uploadGuideSchema = json.RawMessage(`{
	"type": "object",
	"properties":{
		"source":{"type":"string","description":"Local file path on the operator machine to upload."},
		"destination":{"type":"string","description":"Absolute destination path on the helper machine (must be under allowed_dirs)."}
	},
	"required":["source","destination"],
	"additionalProperties":false
}`)

// dbOpSchema is shared by every dbop tool. The connector name (esb /
// erp) is fixed per tool — there is no "type" parameter because the
// driver is decided by which tool the operator picks, exactly like
// how Navicat surfaces one connection per toolbar button.
var dbOpSchema = json.RawMessage(`{
	"type":"object",
	"properties":{
		"sql":{"type":"string","description":"SQL script. Multi-statement OK; statements separated by ';' outside string literals and comments."},
		"max_rows":{"type":"integer","description":"Per-query row cap. Default 1000, max 5000. Excess rows trigger truncation.","minimum":1,"maximum":5000},
		"statement_timeout":{"type":"integer","description":"Per-statement timeout in seconds. Default 30.","minimum":1,"maximum":600},
		"timeout":{"type":"integer","description":"Total script timeout in seconds. Default 120.","minimum":1,"maximum":3600}
	},
	"required":["sql"],
	"additionalProperties":false
}`)

// dbOpToolDefs is the catalog of database tools. Each entry only
// shows up in Tools() if its connector has been registered via
// WithDB. Adding a new database is one new entry here + one call to
// WithDB in cmd/helper/main.go.
var dbOpToolDefs = []Tool{
	{
		Name: "esb_db_operator",
		Description: "Run a SQL script against the ESB MySQL 8 connector configured in config.json's \"esb\" block. " +
			"Statements starting with SELECT / SHOW / DESC / EXPLAIN / WITH / VALUES / TABLE are run as queries and " +
			"return rows; everything else runs in its own committed transaction and returns affected_rows. Multi-statement scripts are " +
			"supported (semicolon-separated); execution stops at the first error and the partial result is returned. " +
			"Do not include BEGIN/COMMIT/ROLLBACK: each non-query statement is already atomic. Read queries retry once on stale connections; writes are never replayed automatically.",
		InputSchema: dbOpSchema,
	},
	{
		Name: "erp_db_operator",
		Description: "Run a SQL script against the ERP connector configured in config.json's \"erp\" block. The " +
			"driver is chosen by config.json (\"type\": \"oracle\" | \"sqlserver\"); the same call shape works for " +
			"both. Statements starting with SELECT / SHOW / DESC / EXPLAIN / WITH / VALUES / TABLE return rows; " +
			"everything else runs in its own committed transaction and returns affected_rows. Multi-statement scripts (semicolon-separated) stop at the first " +
			"error. Oracle PL/SQL blocks (DECLARE/BEGIN or CREATE TRIGGER/PROCEDURE/FUNCTION/PACKAGE/TYPE) retain internal semicolons; an optional SQL*Plus '/' line is removed before execution. " +
			"Do not include transaction-control BEGIN/COMMIT/ROLLBACK. Read queries retry once on stale connections; writes are never replayed automatically.",
		InputSchema: dbOpSchema,
	},
}
