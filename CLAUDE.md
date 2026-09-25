# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

`AGENTS.md` is the canonical, more detailed agent guide (written for OpenCode but applies equally here) — MCP driving recipes, exec/write_file recipes, upload protocol details. `README.md` is the operator manual (Chinese). `docs/` has 需求/设计/实施计划 docs. When this file and `AGENTS.md` disagree, `AGENTS.md` is usually fresher.

## What this is

A Go toolset for remote ops on customer Windows machines running Tomcat:

```
[customer box]                 [operator box]                        [agent]
helper.exe ──reverse-wss──▶ cloudflared tunnel ◀──HTTPS── server.exe ◀──HTTP── mcpstdio.exe
 (runs tools)                  (wss relay :8443 / plain ws :8444)       (stdio→HTTP, :7777)
```

One `server.exe` serves many helpers (each with a unique `client_id`); each agent workspace pins its helper via `MCP_CLIENT_ID`. The agent-facing HTTP bridge on `127.0.0.1:7777` exposes `/command`, `/health`, `/clients`, `/upload`, `/mcp/tools`, `/mcp/call`.

## Commands

```bash
go test -count=1 -timeout 60s ./...    # all tests
go test ./internal/execshell           # single package
go test -run TestDecode ./cmd/server   # single test
go vet ./...

go build -ldflags="-s -w" -o bin/helper.exe ./cmd/helper
go build -ldflags="-s -w" -o bin/server.exe ./cmd/server
go build -ldflags="-s -w" -o bin/mcpstdio.exe ./cmd/mcpstdio
go run ./cmd/gencert                   # one-time self-signed certs into certs/
```

- `build.bat` / `Makefile` build both main binaries + run tests.
- Go: go.mod requires 1.25. Sole network dep set: gorilla/websocket + DB drivers (go-sql-driver/mysql, go-mssqldb, sijms/go-ora).
- On China networks: `go env -w GOPROXY=https://goproxy.cn,https://proxy.golang.org,direct`.
- Local smoke: `run-server.ps1` in one terminal, `run-helper.ps1` in another, then `curl -k -X POST http://127.0.0.1:7777/command -d '{"action":"status"}'`.

## Architecture invariants (do not regress)

**Tools live on the helper; the server is a dumb relay.** Helper connects via wss, sends `Hello` + `ToolList`; server caches the tool list keyed by `client_id` and relays JSON-RPC `tools/call` round-trips. Adding/changing a tool means editing helper code only — if you find yourself adding per-tool logic in `cmd/server`, undo it. The sanctioned exception is **server-local tools** (e.g. `report_problem` in `cmd/server/server_tools.go`): they run inside server.exe with zero helpers connected, are advertised under the top-level `server_tools` key of `/mcp/tools`, and are intercepted by `/mcp/call` before client_id resolution.

**Package layout:**

- `internal/protocol/` — Envelope/Hello/ToolList/Command/Result JSON types shared by both sides.
- `internal/actions/` — all helper tools: `Registry.Dispatch` (actions.go), implementations (impl.go, text_file.go, http_call.go, upload.go), `Tools()` metadata + JSON Schemas (tools.go). The `esb_db_operator`/`erp_db_operator` tool defs are wired in `tools.go` but only registered when the matching DB config section exists.
- `internal/dbop/` — DB access for those tools (MySQL/Oracle/SQL Server via the three drivers).
- `internal/execshell/` — controlled shell: `Runner` interface, production `OSRunner`, command blacklist, audit.
- `internal/config/` — helper `config.json` load + `Validate`.
- `internal/wsclient/` / `internal/wsserver/` — websocket layers (reconnect+heartbeat / Hello-Result-ToolList routing), tested against `httptest.Server`.
- `cmd/helper`, `cmd/server`, `cmd/mcpstdio`, `cmd/gencert`.

**Testability design:** every OS-touching surface is an interface so unit tests use fakes — `execshell.Runner` (tests use `fakeRunner`), `actions.Probe` (production `OSProbe` uses PowerShell; tests seed `fakeProbe`), and real gorilla/websocket against `httptest.Server`. `cmd/helper/dispatch_test.go` exercises real dispatch with `t.TempDir()`. Follow this pattern in new code.

**Adding a helper tool:** implement the action as a method on `*Registry` in `internal/actions/`, add a `Registry.Dispatch` case in actions.go, add a `Tool` entry in tools.go (keep schemas flat — no `oneOf`/`anyOf`), test with fakeProbe/`t.TempDir()`/fakeRunner (pattern in `internal/actions/actions_test.go`), rebuild helper only, and bounce the helper so the server re-pulls its ToolList. Full deploy checklist below.

## Hard rules

- **Deploying a new helper.exe (full checklist in AGENTS.md):** after rebuilding, ALWAYS sync the staging dir `Copy-Item -Force .\bin\helper.exe D:\tools\helper-client\helper.exe` and verify sha256 before shipping to the customer box (`D:\bs-esb\helper-client\`), then restart helper there. A stale staging copy has caused real "wrong binary on the customer box" bugs. config.json changes need only the restart + startup-log check (no hot reload exists) — expect `esb_db_operator connected` / `erp_db_operator connected` lines.
- **dbop scripts:** non-query statements run in an explicit per-statement transaction — never put standalone `BEGIN`/`COMMIT`/`ROLLBACK` in scripts. Reads may retry once on stale connection; writes are deliberately never auto-replayed. Oracle PL/SQL blocks are one statement; a SQL*Plus `/` on its own line terminates and is stripped.
- **`exec`** runs via `cmd /C` on Windows: no shell redirects (`2>&1`) or pipes (Go runs the program directly); `work_dir` must exist and be in the helper's `allowed_dirs` whitelist (`work dir not allowed` is correct behavior, not a bug); for complex scripts pass `"shell":"powershell"` (EncodedCommand) or write a .ps1 via `write_file` and run it. Blacklist blocks `rd`/`rmdir` recursive deletes and words like `format` (which also catches PowerShell `Format-Table`). Use `list_exec`/`kill_pid` to manage stray commands.
- **Remote text files:** use `read_file`/`replace_text`/`write_file` MCP tools (atomic, UTF-8 no BOM, allowed_dirs-enforced, ≤1 MiB), not exec echo acrobatics. For big binaries use the server's `POST /upload` (64 KB chunks + sha256 + atomic rename), never base64 through tool payloads.
- **`/mcp/call` semantics:** application errors (unknown tool, timeout) come back as JSON-RPC `error` envelopes with HTTP 200; only malformed frames get non-200. Clients must inspect `response.error`, not `response.status_code`.
- When driving the helper through the `tomcat-helper` MCP server: work meant to happen on the helper machine goes through MCP tools, not local bash. If something can't be done via MCP, do NOT silently fall back — log it via the server-local `report_problem` tool (appends to `problem.md`) or append to `todo.md`, tell the user, and continue with what's possible. After fixing problems logged in `problem.md`, clear resolved entries but preserve the header + append marker.

## Common traps

- `go build ./...` ignores `-ldflags`; use the explicit build commands. Hidden-window build needs `-H windowsgui`.
- Default helper `work_dir` is `cfg.tomcat_home`; on machines without Tomcat there, exec fails with chdir errors — always pass `work_dir` explicitly.
- Windows paths inside JSON tool args must use `\\` escaping.
- PowerShell 5.1 can't `ExportRSAPrivateKey` — use `cmd/gencert`, not `gen-cert.ps1`.
- `cfg.Validate` backfills zero-value timeouts; tests constructing `HelperConfig` directly don't need heartbeat fields.
- `t.TempDir()` writes <20 ms apart can share mtimes — `time.Sleep(20ms)` when tests sort by mtime.
- If curl to the bridge returns `bad request` / `action is required`, `bin\server.exe` is stale — rebuild.
