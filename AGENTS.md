# AGENTS.md — tomcat-ops-helper

Project-specific guidance for OpenCode sessions. Global toolchain notes (Maven,
JDK) live in `~/.config/opencode/AGENTS.md` and do **not** apply here.

## What this is

A small Go program for temporary remote ops on a customer Windows Server running
Tomcat. Reverse-wss helper runs on the customer box; operator runs `server.exe`
locally and drives it via HTTP. See `docs/需求文档.md`, `docs/设计文档.md`,
`docs/实施计划.md`.

## Layout

```
cmd/helper/    customer-side process (reverse-wss client)
cmd/server/    operator-side wss relay + HTTP bridge
cmd/server/server_tools.go   server-local MCP tools (report_problem)
cmd/mcpstdio/  MCP-over-stdio bridge → server.exe /mcp/* (for MCP Inspector / opencode)
cmd/gencert/   one-shot self-signed cert generator for local testing
todo.md        live list of MCP capability gaps surfaced during testing
problem.md     usage-friction log written by the server-local report_problem tool
internal/protocol/   Envelope / Hello / ToolList / Command / Result JSON types
internal/config/     config.json load + Validate
internal/execshell/  controlled shell: Runner interface, blacklist, audit
internal/actions/    named actions + Tool / Tools() metadata; exec is here too
internal/wsclient/   wss client with reconnect + heartbeat + PostConnect hook
internal/wsserver/   wss server with Hello / Result / ToolList routing
```

Three binaries: `helper.exe`, `server.exe`, plus `gencert` is `go run`'d.
`mcpstdio` is `go run`'d (it is intended to be launched by MCP Inspector).

Helper tools live on the helper. Server only relays — it caches each helper's
`ToolList` and exposes them via JSON-RPC. Adding a new helper tool means
**only** editing helper, never server.

Exception: server-local tools (currently just `report_problem`) run inside
`server.exe` with no helper round-trip and work with **zero helpers
connected**. They live in `cmd/server/server_tools.go`, are advertised under
the top-level `server_tools` key of `/mcp/tools`, and are intercepted by
`/mcp/call` before any client_id resolution. See `problem.md` for the log they
write.

## MCP test rules (read this when driving the helper via MCP)

When this project is being driven through the `tomcat-helper` MCP server (via
Inspector, opencode, or any other MCP client), follow these rules:

- **Anything meant to happen on the helper machine goes through MCP**, not
  through opencode's built-in bash/read/write/edit. Concretely: shelling out
  on the helper box, reading helper logs, restarting services, listing
  processes — all via MCP tools.
- **Repository-internal work** (editing `*.go`, running `go test`, reading
  `AGENTS.md`, etc.) uses opencode's built-in tools as usual.
- **If a task can't be done via MCP** (tool missing, blocked by
  `allowed_dirs`, helper offline, ...), do **not** silently fall back to
  bash. Instead append a line to `todo.md`:

  ```
  - [YYYY-MM-DD] <what's missing> — <expected tool name or root cause>
  ```

  Tell the user briefly that it was logged, then keep doing whatever else is
  possible. `todo.md` is a temporary capability-gap log; collapse it into the
  real requirements once testing settles.
- **Feedback goes through `report_problem`.** If a tool is missing, awkward,
  or misbehaving, call the server-local `report_problem` tool (works even when
  no helper is connected) instead of only mentioning it in chat or silently
  working around it. Entries are appended to `problem.md` (same directory as
  `todo.md`), which the operator reviews periodically.
- **Clear resolved problems.** After every item in `problem.md` has been
  implemented and verified, remove the completed entries from `problem.md` so
  it contains only pending work. Always preserve the explanatory header and
  its `report_problem` append marker; never delete or empty the file itself.
- For the `exec` tool: Windows paths in arguments **must** use `\\` (JSON
  escaping). The bridge itself enforces `work_dir` against the helper's
  `allowed_dirs` whitelist — getting `work dir not allowed` is correct
  behaviour, not a bug.

## Build & test

- Go 1.21+ required (developed against 1.27.1).
- `build.bat` builds both binaries into `bin\` and runs tests.
- `Makefile` mirrors the same for bash users.
- One external dep: `github.com/gorilla/websocket v1.5.3`.
- `GOPROXY` must be reachable. On China networks, `go env -w GOPROXY=https://goproxy.cn,https://proxy.golang.org,direct`.
- After pulling deps, `go mod tidy` may need to run once.

```bash
go test -count=1 -timeout 60s ./...   # all tests in ~12s
go test ./internal/execshell          # single package
go test -run TestDecode ./cmd/server  # single test
go vet ./...
go build -ldflags="-s -w" -o bin\helper.exe .\cmd\helper
go build -ldflags="-s -w" -o bin\server.exe .\cmd\server
go run .\cmd\gencert                  # writes certs\server.pem and server.key
```

## Testability design (do not regress)

Every OS-touching surface has an interface so unit tests use fakes:

- `execshell.Runner` — production `OSRunner`; tests use `fakeRunner` that maps
  command→Result. See `internal/execshell/execshell_test.go`.
- `actions.Probe` — production `OSProbe` (PowerShell on Windows); tests use
  `fakeProbe` to seed `[]JavaProcess` / `[]Port`.
- `wsclient.Client` and `wsserver.Server` are tested against `httptest.Server`
  with real `gorilla/websocket` connections.
- `cmd/helper/dispatch_test.go` exercises the real dispatch function with a
  `t.TempDir()` and a real `exec.Exec` — the closest thing to a smoke test
  without standing up the full helper/server pair.

`execshell.Result` carries `Stdout` / `Stderr` / `ExitCode` / `Took` /
`Truncated`. `execshell.AuditEntry` is what the auditor sees; tests can pass a
function instead of writing to disk.

## Runtime conventions

- HTTP bridge listens on `agent_listen` (default `127.0.0.1:7777`):
  - `POST /command` — body is JSON or form-encoded; response is the result
    envelope. Sniffs body for JSON before falling back to form.
  - `GET /clients`, `GET /health`.
  - `GET /mcp/tools` — `?client_id=c-1` for one helper, omit for aggregated
    list across every connected client. Both shapes carry a top-level
    `server_tools` array (currently just `report_problem`); `clients[].tools`
    stays helper-only.
  - `POST /mcp/call` — JSON-RPC 2.0 `tools/call`. Body:
    ```json
    {"jsonrpc":"2.0","id":"1","method":"tools/call",
     "params":{"name":"status","arguments":{},"client_id":"c-1","timeout":30}}
    ```
    `client_id` is optional only when exactly one helper is connected.
    Unknown tool names are rejected with `-32602` before any WS round-trip.
    Server-local tools (names in `server_tools`) ignore `client_id`, run
    in-process, and work with zero helpers connected.
- wss server listens on `listen` (default `:8443` in this repo's `server.json`
  to avoid needing admin for `:443`). Helper connects via `server_url`.
- Optional `ws_listen` (e.g. `127.0.0.1:8444`) starts a second **plain WS**
  listener on the same `wsHandler` — used when fronting the server with
  Cloudflare quick tunnels (`cloudflared tunnel --url http://localhost:8444`),
  since cloudflared does not accept `wss://` as an origin scheme. Empty by
  default; set in `server.json` only when tunneling.
- Helper connects, sends `Hello` then `ToolList` (advertising its tools).
  Server caches the tool list keyed by `Hello.ClientID`. The temporary `c-N`
  id assigned at upgrade is replaced by `Hello.ClientID` so `/mcp/call`
  parameters match.
- `server.json` may set `problem_log` (default `problem.md`, resolved against
  server.exe's working directory). That is the file `report_problem` appends
  to; the helper never sees it.
- Token in `config.json` **must match** a token in `server.json`'s `tokens[]`.
- Helper logs go to stdout and to `helper.log` (audit). Server writes
  `audit.log` JSONL.
- `exec` calls are independent and may run concurrently. On Windows, context
  timeout runs `taskkill /T /F` against the spawned command and bounds pipe
  cleanup, so a foreground daemon cannot block later exec calls. Use
  `kill_pid` for an arbitrary leftover PID; it defaults to killing the process
  tree and refuses to kill helper itself.
- Database writes run in an explicit per-statement transaction. Do not put
  standalone transaction-control `BEGIN` / `COMMIT` / `ROLLBACK` into dbop scripts. Read queries may retry
  once after a stale connection; writes are deliberately never replayed
  automatically because a lost commit response makes their outcome unknown.
- Oracle PL/SQL blocks are accepted as one statement even with internal
  semicolons. A SQL*Plus `/` on its own line terminates the block and is
  stripped before execution; it is not Oracle SQL and must not reach go-ora.
- **Read-only lock (`forbid_cmd`)**: setting `"forbid_cmd": true` inside the
  `esb` or `erp` config.json block locks that connector to read-only —
  `SQLOp.Exec` refuses everything, and Query additionally enforces a narrow
  allowlist (`SELECT` / `SHOW` / `DESC` / `DESCRIBE` / `EXPLAIN SELECT` /
  `EXPLAIN ANALYZE SELECT`), so `WITH … DELETE`-style statements that the
  router would normally treat as queries are rejected too, as are
  `EXPLAIN DELETE` and `SELECT … INTO OUTFILE`. Default false; an absent key
  changes nothing. Meant to be flipped by hand on the customer box to protect
  production data; takes effect after a helper restart. A locked connector
  announces itself in the tool description ("READ-ONLY: …"), in the startup
  log (`… connected (user@host:port/db, read-only)`), in
  `validate_config` (`connectors.<name>.read_only`), and in every tool
  response (`read_only: true`). Policy rejections do not count as connector
  failures (`status.last_error` stays clean).
- **Exec deadlines (2026-09-25 fix)**: `exec` applies `cfg.command_timeout`
  only when the caller context carries **no** deadline. `context.WithTimeout`
  takes min(parent, d), so the old unconditional wrap silently capped
  stop/start waits at the 30s command default even when the caller asked for
  more. Named actions that pass their own deadline (stop_service, deploy, …)
  now get exactly what they ask for.
- **JVM enumeration on sick-WMI boxes**: `probe.JavaProcesses` tries wmic →
  PowerShell `Get-CimInstance` → `tasklist /FO CSV /NH` (name+PID only, no
  cmdline), each with a 15s bound, falling through on zero rows. A total
  failure no longer blanks `status` — it surfaces as
  `java_probe_error`. Waits treat `sc query` STOPPED as authoritative
  confirmation that the JVM is down (`stop_service`, `wait_for_catalina`
  note the fallback), and `looksLikeJVM` ignores the always-running procrun
  monitor (`<Service>w.exe`) plus non-Java processes whose *cmdline* merely
  mentions catalina (an operator reading logs is not the JVM).
- **start_service is idempotent**: `sc start` returning 1056 ("already
  running") re-checks the service state; RUNNING or START_PENDING→RUNNING is
  reported as success with `already_running: true` instead of an error.
- **deploy runs as a tracked task**: every deploy registers a
  `task_id` (reply carries it, sync or async). With `"async": true` the call
  returns a running handle immediately — poll `task_status` (with the id, or
  omit it to list recent tasks) for step-level progress
  (stop_service → replace_jar → start_service) and the final state. The
  background worker runs detached from the caller's transport on a 30-minute
  budget, so an MCP-timeout mid-cycle no longer aborts the box in a
  stopped state; recover by polling `task_status` with the task_id from the
  (timed-out) call's listing.
- Default helper.exe is built **with console** for visibility. For stealth,
  rebuild with `-ldflags="-H windowsgui -s -w"`. `run-helper.ps1` has the note.

## Local end-to-end smoke

```powershell
go run .\cmd\gencert                       # one-time
.\run-server.ps1                            # one terminal
.\run-helper.ps1                            # another terminal
curl -k -X POST http://127.0.0.1:7777/command -d '{"action":"status"}'
curl -k -X POST http://127.0.0.1:7777/command -d '{"action":"list_logs"}'
curl -k -X POST http://127.0.0.1:7777/command -d '{"action":"exec","params":{"cmd":"dir D:\\workspace","work_dir":"D:\\workspace"}}'

# MCP-style discovery + call
curl -k http://127.0.0.1:7777/mcp/tools
curl -k -X POST -H "Content-Type: application/json" ^
  http://127.0.0.1:7777/mcp/call ^
  -d "{\"jsonrpc\":\"2.0\",\"id\":\"1\",\"method\":\"tools/call\",\"params\":{\"name\":\"status\",\"client_id\":\"helper-sijing\"}}"

# Server-local tool: works with zero helpers connected; appends to problem.md.
curl -k -X POST -H "Content-Type: application/json" ^
  http://127.0.0.1:7777/mcp/call ^
  -d "{\"jsonrpc\":\"2.0\",\"id\":\"2\",\"method\":\"tools/call\",\"params\":{\"name\":\"report_problem\",\"arguments\":{\"description\":\"what was awkward\",\"source\":\"my-agent\"}}}"
```

If curl returns `bad request` or `action is required`, the server binary is
stale — rebuild with `go build -ldflags="-s -w" -o bin\server.exe .\cmd\server`.

## Debugging tools with MCP Inspector

`server.exe` exposes a custom HTTP bridge (`POST /mcp/call`), which is **not**
the wire protocol MCP Inspector speaks natively. `cmd/mcpstdio` is a tiny
stdio→HTTP relay that translates between MCP's newline-delimited JSON-RPC on
stdin/stdout and the project's `/mcp/*` endpoints, so Inspector can list and
call every helper tool through its web UI.

The bridge handles `initialize`, `tools/list`, `tools/call`, `ping`, and
notifications. Unknown methods return `-32601`. If more than one helper is
connected, set `MCP_CLIENT_ID` (or `-client`) to disambiguate; otherwise the
single helper is auto-selected.

```powershell
# 1. Start server.exe and helper.exe as in the smoke section above.

# 2. Launch Inspector against the bridge. Inspector opens its web UI on
#    http://localhost:6274 and starts mcpstdio for you.
npx @modelcontextprotocol/inspector go run .\cmd\mcpstdio

# 3. In the Inspector UI:
#    - Tools tab: every helper tool is listed with its inputSchema.
#    - Pick a tool, fill in arguments, click "Run".
#    - Console / Logs panel shows the live JSON-RPC traffic both ways.
```

Useful flags / env for the bridge (set them in Inspector's "Environment
Variables" or via `npx ... -- -client helper-A`):

- `-url` / `MCP_HTTP_URL` — base URL of `server.exe` (default `http://127.0.0.1:7777`).
- `-client` / `MCP_CLIENT_ID` — pin to one helper (skip auto-detect).

The bridge itself is `go run`'d for development; to install:

```powershell
go build -ldflags="-s -w" -o bin\mcpstdio.exe .\cmd\mcpstdio
```

## HTTP calls via MCP

Use `http_call` for HTTP requests from the helper machine; curl is not required.
Pass headers directly in `headers`, including `Authorization`, `X-Access-Token`,
`Cookie`, and `X-Sign`. The helper accepts credential values directly and sends
them unchanged. `headers_file` and `token_file` are optional alternatives, not
prerequisites. Do not require a credential-file write or a specific file-path
confirmation merely to use this tool. See `docs/http_call.md` for examples.

## Writing files via MCP (recipes that actually work)

Remote text files can be read with `read_file {path}` and edited with
`replace_text {path, old_text, new_text}`. Both enforce allowed_dirs and accept
UTF-8 files up to 1 MiB. Replacement is literal and atomic, preserves BOM/CRLF,
and requires exactly one match by default. Set `expected_count` explicitly for
multiple matches; pass `expected_sha256` from read_file to reject stale edits.
Local Read/Edit tools operate on the operator machine, not the remote helper.

Use `list_exec` to inspect active commands and their PIDs, then `kill_pid` to
stop one. The configured total timeout terminates Windows process trees even
when they produce no output; output silence alone is not considered a hang.
Nonzero exec results include `error_summary` as well as stdout/stderr.
Procrun switches such as `//IS//name`, `//RS//name` and `//DS//name` are not paths.
Quote paths with spaces and pass `work_dir` explicitly when needed. Both `rd`
and `rmdir` recursive deletion remain blocked, including reversed /Q /S flags.
For PowerShell use `shell: powershell` (EncodedCommand), or write a .ps1 and
invoke `powershell -NoProfile -File "path"`; avoid nesting -Command quotes in cmd.
After deployment, restart the customer helper to receive these new tools.

For **text** files (configs, scripts, notes) use the `write_file` named
action — it's atomic, no-BOM, no cmd.exe pain:

```json
{
  "name": "write_file",
  "arguments": {
    "path": "E:\\tomcat-helper\\server.xml",
    "text": "<?xml version=\"1.0\"?>...",
    "overwrite": true
  }
}
```

- Default `overwrite=false` (refuses to replace an existing file).
- Always UTF-8 **no BOM** — safe for Tomcat XML, Java properties, etc.
- Atomic via `<path>.tmp` + fsync + rename.
- Returns `{path, bytes, sha256, overwritten}` so the caller can verify.
- For files larger than a few hundred KB, use `exec` + `EncodedCommand`
  (recipes below) or wait for `upload_file` (planned, see todo.md).

### When you must use `exec` for writes

The `exec` tool runs `cmd /C` on Windows. Two recipes tested in
`E:\tomcat-helper\`; both round-trip UTF-8 / Chinese through MCP:

### 1. PowerShell with `-EncodedCommand` (preferred over `-Command`)

Script → UTF-16LE → base64 → pass as `-EncodedCommand`. Script body itself
also base64-encodes any user-supplied content, so quotes / backslashes /
newlines are safe. Path with Chinese works without escape acrobatics.

```json
{
  "cmd": "powershell -NoProfile -NonInteractive -EncodedCommand <BASE64>",
  "work_dir": "E:\\tomcat-helper"
}
```

To build the base64 from a UTF-8 script on the operator box:

```powershell
$bytes = [Text.Encoding]::Unicode.GetBytes($script)   # UTF-16LE
[Convert]::ToBase64String($bytes)
```

For an in-memory blob (e.g. write Chinese text to disk without temp files):

```powershell
$script = @"
`$content = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('<CONTENT_B64>'))
[IO.File]::WriteAllText('E:\tomcat-helper\jingyesi.txt', `$content, (New-Object Text.UTF8Encoding(`$false)))
"@
```

### 2. Avoid

- `powershell -Command "..."` under `cmd /C`: cmd can eat quotes /
  backslashes mid-pipe and sometimes print the entire command to stdout
  as if it were `echo`.
- `[Text.UTF8Encoding]::UTF8` writes a UTF-8 BOM (`\uFEFF`). Harmless for
  `tail` / `read_tail`, but if downstream tools reject BOM, use
  `New-Object Text.UTF8Encoding($false)` instead.

### 3. `work_dir` matters

The default `work_dir` is `cfg.tomcat_home`. If that path does not exist
on the host (e.g. local test machine without Tomcat installed), `cmd /C`
fails with `chdir ... The system cannot find the file specified.` Always
pass `work_dir` explicitly to a path that exists on the helper box, and
make sure that path is in `allowed_dirs`.

## Uploading big files (jar / war / any binary)

For anything bigger than a few hundred KB, do **not** push the bytes
through the LLM's tool call payload — use the server's `POST /upload`
endpoint instead. The server chunks the file into 64 KB pieces, sends
each as an `upload_file` command to the helper, waits for the helper's
per-chunk Result, and only returns the final `Result` to the operator.

The helper writes to `<path>.tmp`, fsyncs every chunk, verifies the
optional `sha256` on the final chunk, and atomically renames to the
final path. The destination must be in `allowed_dirs`; default is to
refuse overwriting an existing file.

```powershell
# Compute hash on the operator box.
$sha = (Get-FileHash .\app.jar -Algorithm SHA256).Hash.ToLower()

# Upload. -F builds multipart/form-data with a "file" part from app.jar.
curl.exe -k -X POST `
  -F "file=@.\app.jar" `
  -F "path=E:\tomcat-helper\app.jar" `
  -F "sha256=$sha" `
  -F "overwrite=true" `
  https://server:7777/upload
```

Response shape:

```json
{
  "upload_id": "...",
  "path": "E:\\tomcat-helper\\app.jar",
  "bytes": 52428800,
  "duration_ms": 12453,
  "result": {"bytes":52428800,"sha256":"...","verified":true,"overwritten":false}
}
```

Notes:

- Path with Chinese characters is fine — go straight through multipart,
  no cmd / powershell quote acrobatics.
- `client_id` is optional when exactly one helper is connected; required
  otherwise.
- `timeout` is per-chunk in seconds (default 30). For a 50 MB upload
  expect ~80 round-trips of 64 KB chunks — most of the time is server
  ↔ helper latency, not the bytes themselves.
- The internal `upload_file` action is intentionally **not advertised** in
  MCP `tools/list`. It exists only as the helper-side chunk protocol driven by
  server.exe's `POST /upload`; agents must use the HTTP endpoint above instead
  of sending base64 chunks through tool calls.

## Adding or changing a tool (do this on helper only)

Tools live entirely on the helper. The server only knows what each helper
advertised in its `ToolList`. To ship a new capability:

1. **Implement the action** in `internal/actions/impl.go` (or a new file in
   the same package). Each action is a method on `*Registry` taking
   `ctx` + `json.RawMessage` params and returning `(json.RawMessage, error)`.
2. **Add a dispatch case** to `Registry.Dispatch` in `internal/actions/actions.go`.
3. **Add a `Tool` entry** in `internal/actions/tools.go` with `Name`,
   `Description`, and a JSON Schema for `InputSchema`. Keep schemas flat
   (no `oneOf`/`anyOf` unless necessary — most clients struggle with them).
4. **Test it**: add unit tests using `fakeProbe` / `t.TempDir()` /
   `fakeRunner`. Pattern: see `internal/actions/actions_test.go`.
5. **Rebuild helper only**: `go build -ldflags="-s -w" -o bin\helper.exe .\cmd\helper`.
   Server is unchanged; it will pick up the new ToolList the next time the
   helper reconnects (or just bounce helper in place).
6. **Sync staging dir**: `Copy-Item -Force .\bin\helper.exe D:\tools\helper-client\helper.exe`
   and verify sha256 matches. This directory is what gets handed to the customer box.

If you accidentally edit server code thinking a new helper tool belongs
there — undo that. The server has no per-helper-tool logic; it only relays.

The one sanctioned exception is a **server-local tool** (e.g.
`report_problem` in `cmd/server/server_tools.go`) that must work with no
helper online. Add those to `serverToolDefs()`, handle them in
`executeServerTool()`, and test them in `cmd/server/problem_test.go`.

## Common traps

- `go build ./...` ignores the `-ldflags` defaults; helper.exe will get the
  console by default. Use the explicit command above to flip `-H windowsgui`.
- `fmt.Sprintf` format strings with `%1` look like vet warnings; use `%%1`.
- PowerShell 5.1 cannot export RSA private keys via `ExportRSAPrivateKey`;
  use `cmd/gencert` instead of `gen-cert.ps1`.
- `web-proxy.golang.org` is often blocked from China; set `GOPROXY` before
  the first `go mod tidy`.
- Server's `tls_cert`/`tls_key` must be PEM-encoded. `cmd/gencert` writes both.
- `cfg.Validate` falls back to defaults for zero-value timeouts/sizes; tests
  that build `HelperConfig` directly do **not** need to set heartbeat fields.
- `wsclient.Options.WriteTimeout` defaults to 10s; if you ever override it
  with `0` the heartbeat write will fail with `i/o timeout` every 30s.
- The `dispatch` in `cmd/helper/main.go` does not return errors to the WS
  layer; failures become `Result{Success:false, Error: msg}`.
- Tests touching `t.TempDir()` with sub-20ms gaps can produce identical mtimes;
  add `time.Sleep(20ms)` between writes when sorting by mtime.
- `/mcp/call` returns JSON-RPC `error` envelopes (HTTP 200) for *application*
  errors (unknown tool, timeout, send failure). It only returns non-200 HTTP
  for malformed JSON-RPC frames themselves (`-32700` parse error). Clients
  must inspect `response.error`, not `response.status_code`.

## Deploying a new helper.exe

Whenever helper source changes (new tool, bug fix, config reload
support, etc.), go through this checklist end-to-end. Skipping steps
leads to "I edited config.json but the new password doesn't work"
ghost bugs.

1. **Local build + test** — `go test -count=1 -timeout 60s ./...`
   then `go build -ldflags="-s -w" -o bin\helper.exe .\cmd\helper`.
2. **Sync operator staging dir (REQUIRED)** — copy the freshly built
   `bin\helper.exe` over `D:\tools\helper-client\helper.exe` so the
   staging folder always tracks the latest binary. This is the
   directory the operator hands off to the customer box, so it MUST
   be the newest helper.exe — not whatever happened to be in there
   from a previous build. Use the same `Copy-Item` (or your equivalent)
   that step 3 uses, then verify both files match:

   ```powershell
   Copy-Item -Force .\bin\helper.exe D:\tools\helper-client\helper.exe
   $a = (Get-FileHash .\bin\helper.exe).Hash
   $b = (Get-FileHash 'D:\tools\helper-client\helper.exe').Hash
   if ($a -ne $b) { throw "staging helper.exe sha256 mismatch: $a vs $b" }
   ```

   Do not skip this even if "the customer copy looks fine" — the
   staging dir is the only artifact other agents see when handing
   out helpers, and a stale copy there has caused real "wrong binary
   on the customer box" bugs.
3. **Hand off to customer box** — copy to `D:\bs-esb\helper-client\helper.exe`
   on the helper host. Same sha256 expected.
4. **Restart helper** — kill the running `helper.exe` (taskkill /F
   /IM helper.exe, or close the run-helper.ps1 console), then start
   `D:\bs-esb\helper-client\run-helper.ps1`.
5. **Verify startup log** — should see
   `info: esb_db_operator connected (...)` and
   `info: erp_db_operator connected (...)` — a `, read-only)` suffix means
   `forbid_cmd` is active on that connector. If either says
   `startup ping failed: ...`, the config.json password is wrong;
   fix and go back to step 4.
6. **Optional: run `validate_config`** — once it shows up in MCP
   tools, call it to get the same startup snapshot plus every
   allowed_dirs entry existence check. Useful when you can't tell
   whether a config change took effect.

`config.json` changes (`tomcat_home`, `jdk_home`, `log_dir`,
`allowed_dirs`, `esb.*`, `erp.*`) require **steps 4–6** but not
1–3 — helper reads config.json at startup. There is no SIGHUP /
file-watch / hot-reload; a restart is the only way.

## Common traps

- `go build ./...` ignores the `-ldflags` defaults; helper.exe will get the
  console by default. Use the explicit command above to flip `-H windowsgui`.
- `fmt.Sprintf` format strings with `%1` look like vet warnings; use `%%1`.
- PowerShell 5.1 cannot export RSA private keys via `ExportRSAPrivateKey`;
  use `cmd/gencert` instead of `gen-cert.ps1`.
- `web-proxy.golang.org` is often blocked from China; set `GOPROXY` before
  the first `go mod tidy`.
- Server's `tls_cert`/`tls_key` must be PEM-encoded. `cmd/gencert` writes both.
- `cfg.Validate` falls back to defaults for zero-value timeouts/sizes; tests
  that build `HelperConfig` directly do **not** need to set heartbeat fields.
- `wsclient.Options.WriteTimeout` defaults to 10s; if you ever override it
  with `0` the heartbeat write will fail with `i/o timeout` every 30s.
- The `dispatch` in `cmd/helper/main.go` does not return errors to the WS
  layer; failures become `Result{Success:false, Error: msg}`.
- Tests touching `t.TempDir()` with sub-20ms gaps can produce identical mtimes;
  add `time.Sleep(20ms)` between writes when sorting by mtime.
- `exec` runs the user's command via cmd /C on Windows, which means
  paths with spaces get re-tokenized and quoted-away. For complex
  scripts (multi-line, cmdlets, anything with single-quotes), pass
  `"shell":"powershell"` to wrap the command in
  `powershell -NoProfile -NonInteractive -Command "Set-Location '…'; <cmd>"`,
  or write the script to a file via `write_file` and run that.
  `exec` does NOT support shell redirects (`2>&1`) or pipes
  (`cmd | other`) — Go's `os/exec` runs the program directly, no
  shell in between.
- PowerShell variable name collision: `$host`, `$args`, `$input`,
  `$matches`, `$PSItem`, `$null` are reserved. Avoid them as PS script
  variable names. PowerShell single-quoted strings are literal —
  `'D:\bs-esb'` is 8 chars; `'D:\\bs-esb'` is 9 chars. The difference
  matters when matching against file content.
- After every helper rebuild: copy `bin\helper.exe` to
  `D:\tools\helper-client\helper.exe` BEFORE shipping to the customer.
  That staging dir is what other agents see and copy from, so leaving
  it stale turns into "I copied the new build but the customer still
  got the old one" bugs that take hours to chase. Full procedure in
  "Deploying a new helper.exe" above.
