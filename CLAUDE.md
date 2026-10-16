# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository. It is the **canonical agent guide** — `AGENTS.md` is a one-line pointer kept only for tools that look for it by name. `README.md` is the operator manual (Chinese). `docs/` has 需求/设计/实施计划 docs; `docs/http_call.md` has http_call examples.

## What this is

A Go toolset for remote ops on customer Windows machines running Tomcat:

```
[customer box]                 [operator box]                        [agent]
helper.exe ──reverse-wss──▶ cloudflared tunnel ◀──HTTPS── server.exe ◀──HTTP── mcpstdio.exe
 (runs tools)                  (wss relay :8443 / plain ws :8444)       (stdio→HTTP, :7777)
```

One `server.exe` serves many helpers (each with a unique `client_id`); each agent workspace pins its helper via `MCP_CLIENT_ID`.

## Commands

```bash
go test -count=1 -timeout 60s ./...    # all tests
go test ./internal/execshell           # single package
go test -run TestDecode ./cmd/server   # single test
go vet ./...

go build -ldflags="-s -w" -o bin/helper.exe ./cmd/helper
go build -ldflags="-s -w" -o bin/server.exe ./cmd/server
go build -ldflags="-s -w" -o bin/mcpstdio.exe ./cmd/mcpstdio
go build -ldflags="-s -w" -o bin/helper-gui.exe ./cmd/helper-gui
go run ./cmd/gencert                   # one-time self-signed certs into certs/
```

- `build.bat` / `Makefile` build both main binaries + run tests.
- Go: go.mod requires 1.25. Sole network dep set: gorilla/websocket + DB drivers (go-sql-driver/mysql, go-mssqldb, sijms/go-ora).
- On China networks: `go env -w GOPROXY=https://goproxy.cn,https://proxy.golang.org,direct`.

## Architecture invariants (do not regress)

**Tools live on the helper; the server is a dumb relay.** Helper connects via wss, sends `Hello` + `ToolList`; server caches the tool list keyed by `client_id` and relays JSON-RPC `tools/call` round-trips. Adding/changing a tool means editing helper code only — if you find yourself adding per-tool logic in `cmd/server`, undo it. The sanctioned exception is **server-local tools** (e.g. `report_problem` in `cmd/server/server_tools.go`): they run inside server.exe with zero helpers connected, are advertised under the top-level `server_tools` key of `/mcp/tools`, and are intercepted by `/mcp/call` before client_id resolution.

**Package layout:**

- `internal/protocol/` — Envelope/Hello/ToolList/Command/Result JSON types shared by both sides.
- `internal/actions/` — all helper tools: `Registry.Dispatch` (actions.go), implementations (impl.go, text_file.go, http_call.go, upload.go, reload.go), `Tools()` metadata + JSON Schemas (tools.go). The `esb_db_operator`/`erp_db_operator` tool defs are wired in `tools.go` but only registered when the matching DB config section exists.
- `internal/dbop/` — DB access for those tools (MySQL/Oracle/SQL Server via the three drivers).
- `internal/execshell/` — controlled shell: `Runner` interface, production `OSRunner`, command blacklist, audit.
- `internal/config/` — helper `config.json` load + `Validate`.
- `internal/wsclient/` / `internal/wsserver/` — websocket layers (reconnect+heartbeat / Hello-Result-ToolList routing), tested against `httptest.Server`.
- `internal/helpergui/` — the customer-facing local web UI served by helper-gui.exe (config editor, connection tests, launcher, log pane); all handlers are methods on `*Server` tested via httptest.
- `cmd/helper`, `cmd/server`, `cmd/mcpstdio`, `cmd/gencert`, `cmd/helper-gui`.

**Testability design:** every OS-touching surface is an interface so unit tests use fakes — `execshell.Runner` (tests use `fakeRunner`), `actions.Probe` (production `OSProbe` uses PowerShell; tests seed `fakeProbe`), and real gorilla/websocket against `httptest.Server`. `cmd/helper/dispatch_test.go` exercises real dispatch with `t.TempDir()`. Follow this pattern in new code.

**Adding a helper tool:** implement the action as a method on `*Registry` in `internal/actions/`, add a `Registry.Dispatch` case in actions.go, add a `Tool` entry in tools.go (keep schemas flat — no `oneOf`/`anyOf`), test with fakeProbe/`t.TempDir()`/fakeRunner (pattern in `internal/actions/actions_test.go`), rebuild helper only, and bounce the helper so the server re-pulls its ToolList (`reload_config` re-pushes it on a live connection). Full deploy checklist below.

## Runtime conventions

- `restart {async: true}` returns a tracked task_id immediately and continues on a
  detached 30-minute budget. Poll `task_status` for `done`, `restart`, and `error`;
  omitting task_id lists recent deploy and restart tasks. Synchronous restart
  preserves its existing response format.
- `deploy` / `replace_jar` src and dest are both on the helper host. When they
  refer to the same existing file, replacement skips backup/copy and verifies
  the file size/hash before proceeding (upload has already replaced the file).
- `upload_guide` defaults to the operator bridge `http://127.0.0.1:7777`.
  Set its `server_url` argument for a custom HTTP(S) bridge. The helper's WSS
  tunnel address does not determine the operator's upload endpoint.

- HTTP bridge on `agent_listen` (default `127.0.0.1:7777`): `POST /command` (JSON or form-encoded body; response is the result envelope), `GET /clients`, `GET /health`, `GET /upload` (progress), `GET /mcp/tools` (`?client_id=` for one helper, omitted = aggregated; both carry a top-level `server_tools` array), `POST /mcp/call` (JSON-RPC 2.0 `tools/call`; `client_id` is optional only when exactly one helper is connected; unknown tools rejected with `-32602` before any WS round-trip).
- wss server on `listen` (default `:8443` — avoids admin for `:443`). Optional `ws_listen` (e.g. `127.0.0.1:8444`) starts a second **plain-WS** listener on the same handler for cloudflared quick tunnels (`cloudflared tunnel --url http://localhost:8444`) — cloudflared doesn't accept `wss://` origins.
- Helper sends `Hello` then `ToolList`; the server caches tools keyed by `Hello.ClientID` (the temporary `c-N` upgrade id is replaced). Token in helper `config.json` must match a `tokens[]` entry in `server.json`.
- Logs: helper → stdout + `helper.log` (audit); server → `audit.log` JSONL. `server.json` may set `problem_log` (default `problem.md` next to server.exe) — that's where `report_problem` appends; the helper never sees it.
- `exec` calls are independent and may run concurrently. On Windows, timeout runs `taskkill /T /F` on the spawned tree, so a foreground daemon cannot block later execs; output silence alone is not a hang. `kill_pid` kills any PID (tree+force by default) and refuses to kill helper itself.
- **Exec deadlines**: `cfg.command_timeout` applies only when the caller context carries **no** deadline (`context.WithTimeout` takes min(parent, d)); named actions that pass their own deadline (stop_service, deploy, …) get exactly what they ask for.
- **JVM enumeration on sick-WMI boxes**: probe chain wmic → PowerShell `Get-CimInstance` → `tasklist /FO CSV /NH` (15s bound each, zero rows falls through). Total failure surfaces as `java_probe_error` instead of blanking `status`. Waits treat `sc query` STOPPED as authoritative JVM-down confirmation; the procrun monitor (`<Service>w.exe`) and non-Java processes whose cmdline merely mentions catalina are ignored.
- **start_service is idempotent**: `sc start` returning 1056 ("already running") re-checks state; RUNNING is reported as success with `already_running: true`. 1056 during a slow procrun stop (STOP_PENDING — JVM gone but service not yet STOPPED, 2026-10-03 report) waits for STOPPED within the timeout and retries `sc start` once, succeeding with an explanatory `note`.
- **deploy runs as a tracked task** (sync or async — the reply always carries a `task_id`). With `"async": true` it returns immediately; poll `task_status` (omit `task_id` to list recent tasks, e.g. after a timed-out call). The background worker runs detached on a 30-minute budget, so an MCP timeout mid-deploy no longer strands the box in a stopped state.
- **`forbid_cmd` read-only lock**: `"forbid_cmd": true` inside the `esb`/`erp` config block locks that connector read-only — `Exec` refuses everything, and `Query` additionally enforces a narrow allowlist (`SELECT` / `SHOW` / `DESC` / `DESCRIBE` / `EXPLAIN SELECT` / `EXPLAIN ANALYZE SELECT`), so `WITH … DELETE`-style statements the router would call queries are rejected too, as are `EXPLAIN DELETE` and `SELECT … INTO OUTFILE`. A locked connector announces itself in the tool description ("READ-ONLY: …"), the startup log (`… connected (…, read-only)`), `validate_config` (`connectors.<name>.read_only`), and every response (`read_only: true`); policy rejections don't count as connector failures.
- Default helper.exe is built **with console** for visibility; for stealth rebuild with `-ldflags="-H windowsgui -s -w"`.
- **helper-gui.exe** is the customer-side front door: double-click → local Chinese web UI on `127.0.0.1:7788` (random-port fallback; browser auto-opens). Visual config.json editing with server-side directory-tree folder pickers, 测试连接 for DB blocks (reuses `dbop.NewSQLOp` — same DSN logic as production) and for `server_url` (real ws handshake, classifies auth/404/tls/timeout/dial), atomic validated saves that preserve unknown JSON fields, plus a launcher (child helper.exe with piped stdio into a ring; stop = `taskkill /PID n /T /F`; external instances detected via tasklist → log pane tails helper.log instead). All `/api/*` routes require the `X-Requested-With: helper-gui` header (local CSRF guard). **Agent-side config edits still go through `write_file` + `reload_config`** — the GUI is for humans.

## Driving the helper via MCP (rules & recipes)

- **problem.md 自动闭环规则（无需用户再次提醒）**：每次处理反馈后，完成实现和相关验证，立即从 `problem.md` 删除已解决的待办条目；仅保留尚未解决或尚未验证的内容。全部处理完成时，保留文件说明头、`report_problem` 追加标记和“（暂无未处理条目。）”，不要删除或清空整个文件。结束任务前重新读取 `problem.md`，确认已完成条目没有残留；若处理期间有人追加新反馈，保留该反馈，不覆盖。不能只因代码已修改就删除仍待验证的条目。

- Work meant to happen on the helper machine goes through MCP tools, not local bash; repository-internal work (editing `*.go`, running `go test`) uses local tools as usual. If something can't be done via MCP (tool missing, blocked by `allowed_dirs`, helper offline), do NOT silently fall back — log it via the server-local `report_problem` tool (works with zero helpers connected, appends to `problem.md`), tell the user, and continue with what's possible. After fixing problems logged in `problem.md`, clear resolved entries but preserve the header + append marker.
- **http_call** does HTTP from the helper machine; curl is not required. Pass credentials directly in `headers` (`Authorization`, `X-Access-Token`, `Cookie`, `X-Sign`) — they're sent unchanged; `headers_file`/`token_file` are optional alternatives, not prerequisites. Don't demand a credential-file write merely to use this tool. See `docs/http_call.md`.
- **Remote text files**: `read_file {path}` / `replace_text {path, old_text, new_text}` / `write_file {path, text, overwrite?}` / `append_text_file` — atomic, UTF-8 no BOM, allowed_dirs-enforced, ≤1 MiB. replace_text is literal, preserves BOM/CRLF, requires exactly one match unless `expected_count` is set; `expected_sha256` (from read_file) rejects stale edits. write_file refuses to overwrite unless `overwrite=true` and returns `{path, bytes, sha256, overwritten}`. Never write remote files via exec echo acrobatics.
- **exec recipes**: default is `cmd /C` on Windows and `sh -c` on Unix; pipes and redirects (`|`, `>`, `2>&1`) are supported. Redirect targets must stay inside allowed_dirs. For log tails use `tail {path, lines}` or PowerShell `Get-Content -Tail 50`. For PowerShell syntax pass `"shell":"powershell"`: the command is wrapped as `powershell -NoProfile -NonInteractive -EncodedCommand <b64>` (UTF-16LE base64, with a `Set-Location '<work_dir>'` prologue) — quotes/backslashes/newlines/Chinese paths all survive. Build the base64 yourself with `[Text.Encoding]::Unicode.GetBytes($script)` → `[Convert]::ToBase64String(...)`. For multi-step scripts, write a .ps1/.cmd via `write_file` and run it. Avoid `powershell -Command "…"` under cmd (quote-eating). `[Text.UTF8Encoding]::UTF8` writes a BOM — use `New-Object Text.UTF8Encoding($false)` when BOM matters.
- **exec blacklist** blocks `rd`/`rmdir` recursive deletes (including reversed `/Q /S` flags) and words like `format` (which also catches PowerShell `Format-Table`). Procrun switches (`//IS//name`, `//RS//name`, `//DS//name`) are not paths — don't quote them as such. Nonzero exit results carry `error_summary` alongside stdout/stderr.
- **Big binaries** (jar/war, more than a few hundred KB): never push bytes through tool-call payloads. Use the server's `POST /upload` (multipart: `file`, `path`, `sha256`, `overwrite`, `client_id`?, `timeout` = per-chunk seconds, default 30). Server chunks 64 KB; helper writes `<path>.tmp` + fsync per chunk + optional sha256 verify + atomic rename; response carries `{upload_id, path, bytes, duration_ms, result:{bytes, sha256, verified, overwritten}}`. The internal `upload_file` action is intentionally hidden from `tools/list` — agents use the HTTP endpoint; the `upload_guide` helper tool prints the exact curl. Chinese paths go straight through multipart.
- **dbop scripts**: non-query statements run in an explicit per-statement transaction — never put standalone `BEGIN`/`COMMIT`/`ROLLBACK` in scripts. Reads may retry once on stale connection; writes are deliberately never auto-replayed (a lost commit response makes the outcome unknown). Oracle PL/SQL blocks are one statement; a SQL*Plus `/` on its own line terminates the block and is stripped.
- **`/mcp/call` semantics**: application errors (unknown tool, timeout, send failure) come back as JSON-RPC `error` envelopes with HTTP 200; only malformed frames get non-200. Clients must inspect `response.error`, not `response.status_code`.
- Windows paths inside JSON tool args must use `\\` escaping.

## Deploying a new helper.exe

Whenever helper source changes (new tool, bug fix), go through this checklist end-to-end. Skipping steps leads to "I edited config.json but the new password doesn't work" ghost bugs.

1. **Local build + test** — `go test -count=1 -timeout 60s ./...`, then `go build -ldflags="-s -w" -o bin/helper.exe ./cmd/helper`.
2. **Sync operator staging dir (REQUIRED)** — `D:\tools\helper-client\` is the **only** staging entry point (the bundle lived in-repo as `out/helper-client` between 2026-10-01 and 2026-10-10, then moved back out — it carries real-token config.json and 14 MB exes that must never land in git history). It must always track the latest binary; a stale copy there has caused real "wrong binary on the customer box" bugs:

   ```powershell
   Copy-Item -Force .\bin\helper.exe D:\tools\helper-client\helper.exe
   Copy-Item -Force .\bin\helper-gui.exe D:\tools\helper-client\helper-gui.exe
   $a = (Get-FileHash .\bin\helper.exe).Hash
   $b = (Get-FileHash 'D:\tools\helper-client\helper.exe').Hash
   if ($a -ne $b) { throw "staging helper.exe sha256 mismatch: $a vs $b" }
   ```

   The bundle README's tracked source is `deploy/helper-client-README.md` — edit that, then copy it to `D:\tools\helper-client\README.md`. When a config field changes for a deployment, edit `D:\tools\helper-client\config.json` there too (and after rollout, confirm with `validate_config` / `reload_config`).

3. **Hand off to customer box** — copy to `D:\bs-esb\helper-client\helper.exe` on the helper host. Same sha256 expected.
4. **Restart helper** — kill the running `helper.exe` (taskkill /F /IM helper.exe, or close the run-helper.ps1 console), then start `D:\bs-esb\helper-client\run-helper.ps1`.
5. **Verify startup log** — expect `info: esb_db_operator connected (...)` / `info: erp_db_operator connected (...)`; a `, read-only)` suffix means `forbid_cmd` is active. `startup ping failed: ...` = wrong password; fix and redo step 4.
6. **Verify via `validate_config`** — connector health + every allowed_dirs existence check.

`config.json` changes (`tomcat_home`, `jdk_home`, `log_dir`, `allowed_dirs`, `esb.*`, `erp.*`) no longer need a restart: edit the file (via `write_file`), then call the helper's `reload_config` tool — it re-reads + validates config.json, hot-swaps the running config, closes and re-wires the DB connectors, and re-pushes the MCP tool list on the live connection. A parse/validate error aborts the reload and keeps the running config. **Exception:** `server_url` / `token` / heartbeat intervals are captured at process start — changing those still requires steps 4–6. Follow up with `validate_config` to confirm the connectors reconnected.

## Local smoke & MCP Inspector

```powershell
go run .\cmd\gencert                       # one-time
.\run-server.ps1                            # one terminal
.\run-helper.ps1                            # another terminal
curl -k -X POST http://127.0.0.1:7777/command -d '{"action":"status"}'
curl -k http://127.0.0.1:7777/mcp/tools
curl -k -X POST -H "Content-Type: application/json" http://127.0.0.1:7777/mcp/call `
  -d '{\"jsonrpc\":\"2.0\",\"id\":\"1\",\"method\":\"tools/call\",\"params\":{\"name\":\"status\",\"client_id\":\"helper-XXX\"}}'
```

If curl returns `bad request` / `action is required`, `bin\server.exe` is stale — rebuild.

`server.exe`'s `/mcp/*` bridge is not the wire protocol MCP Inspector speaks; `cmd/mcpstdio` is the stdio→HTTP relay. Inspector: `npx @modelcontextprotocol/inspector go run .\cmd\mcpstdio`. Flags/env: `-url`/`MCP_HTTP_URL` (default `http://127.0.0.1:7777`), `-client`/`MCP_CLIENT_ID` (pin one helper when several are connected). The bridge handles `initialize` / `tools/list` / `tools/call` / `ping` / notifications; unknown methods → `-32601`. `mcpstdio` also polls `/mcp/tools` every 5s and emits `tools/list_changed` when the tool-name fingerprint changes.

## `legacy-win2012` branch (frozen)

One customer box (比硕) runs Windows Server 2012 R2, whose loader rejects Go 1.21+ binaries (ERROR_BAD_EXE_FORMAT). That branch builds helper with Go 1.20.14 by full path (`D:/Scoop/apps/go120/current/bin/go.exe build -ldflags="-s -w" -o bin/helper-2012r2.exe ./cmd/helper`), pins old deps in go.mod, and stages to `D:\tools\helper-client-2012r2\` as `helper-2012r2.exe`. **Not routinely maintained** — every other box runs master builds (staged in `D:\tools\helper-client\`). Merge from master only if that box needs a new feature (loop-var semantics revert to pre-1.22 there; re-audit if you add loops that spawn goroutines). Never cross-copy exes between the two staging folders.

## Common traps

- `go build ./...` ignores `-ldflags`; use the explicit build commands. Hidden-window build needs `-H windowsgui`.
- Default helper `work_dir` is `cfg.tomcat_home`; on machines without Tomcat there, exec fails with chdir errors — always pass `work_dir` explicitly (it must exist and be in `allowed_dirs`; `work dir not allowed` is correct behavior, not a bug).
- Windows paths inside JSON tool args must use `\\` escaping.
- PowerShell 5.1 can't `ExportRSAPrivateKey` — use `cmd/gencert`, not `gen-cert.ps1`.
- PowerShell reserved variable names (`$host`, `$args`, `$input`, `$matches`, `$PSItem`, `$null`) — avoid them in scripts. Single-quoted PS strings are literal: `'D:\bs-esb'` is 8 chars, `'D:\\bs-esb'` is 9 — matters when matching file content.
- `fmt.Sprintf` format strings with `%1` look like vet warnings; use `%%1`.
- Server's `tls_cert`/`tls_key` must be PEM-encoded; `cmd/gencert` writes both.
- `cfg.Validate` backfills zero-value timeouts; tests constructing `HelperConfig` directly don't need heartbeat fields.
- `wsclient.Options.WriteTimeout` defaults to 10s; setting it to `0` makes heartbeat writes fail with `i/o timeout` every 30s.
- `dispatch` in `cmd/helper/main.go` never returns errors to the WS layer; failures become `Result{Success:false, Error: msg}`.
- `t.TempDir()` writes <20 ms apart can share mtimes — `time.Sleep(20ms)` when tests sort by mtime.
- If curl to the bridge returns `bad request` / `action is required`, `bin\server.exe` is stale — rebuild.
