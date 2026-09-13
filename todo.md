# MCP 能力 gap 清单（测试期临时记录）

每条一行，格式：`- [YYYY-MM-DD] <缺什么> — <期望工具名 / 根因>`

- [2026-09-13] ~~LLM 调不到 helper 动态工具（`esb_db_operator` / `erp_db_operator` 等）——~~ **已解决**。mcpstdio 加后台 poller（默认 5s）拉 `/mcp/tools`，工具名指纹变了就 emit `notifications/tools/list_changed`，opencode 按 MCP 规范自动重跑 `tools/list`。新增 `-poll-interval` flag + `MCP_POLL_INTERVAL` env。Bridge stdout 写入加 mutex。测试：`TestFingerprintTools` / `TestWriteNotification_Format` / `TestPoller_EmitsOnChange` / `TestPoller_NoChange_StaysQuiet` / `TestPoller_Disabled`。短期 workaround 仍可用（curl `/mcp/call`），但不再需要。

测试期结束后把高频项收敛进正式需求文档。

---


- [2026-09-12] **新增 `upload_file` 大文件传输（jar/war 部署）** —— **已实现**。
  - **协议层**：所有 chunk 状态都在 `Command.Params`（`UploadParams` JSON）里，没污染 `Envelope`，其他消息类型完全不受影响
  - **分块**：server 端 64 KB 一块，base64 嵌入 JSON Command，**顺序发送 + 每块等 Result**（不 pipeline，故障处理简单）
  - **写盘**：helper 写到 `<path>.tmp` + 写每块时累积 SHA-256 + 收尾 `fsync + close + 验 sha256 + atomic rename`
  - **路径**：`allowed_dirs` 白名单，默认禁止覆盖，`overwrite=true` 才允许
  - **断点续传**：MVP 未做（in-memory session map，断线即丢）；后续可加按 `(upload_id, offset)` 持久化
  - **进度上报**：helper 每块返回 `UploadProgressData{bytes_written}`；server 把整个 upload 的 start/done/timeout/send_error 全写 `audit.log`
  - **服务端**：`POST /upload` 接 multipart，`file`/`path`/`sha256`/`overwrite`/`client_id`/`timeout`
  - **响应**：操作员拿到 `{upload_id, path, bytes, duration_ms, result:{...}}` —— 只看得到**最终** Result 的数据
  - **MCP 暴露**：tool 列表里有 `upload_file` 但**故意不让 LLM 直接调**（chunked base64 在 tool-call payload 里烧上下文），用 curl 走 `/upload` 端点
  - **测试**：9 条 helper 单测（单 chunk / 多 chunk 组装 / offset 不匹配 / 拒绝覆盖 / 覆盖替换 / 路径白名单 / 必填字段 / sha256 mismatch 不晋升 / 无 expected_sha256 也 verified=true）+ 1 条 server 端到端测试（71 KB 文件 → 3 chunk → reassemble 一致）
  - **二进制**：`bin\helper.exe` + `bin\server.exe` 已重建（2026-09-12 12:33）
  - **价值**：绕开 RDP / VPN 传文件这套笨办法 —— wss 出站本来就是为了过防火墙

---

# Plan: 数据库直连 MCP 工具（esb_db_operator / erp_db_operator）

> 设计 + 实施 checklist。完成后归档到正式需求 / 设计文档。

## 背景
客户机上有两个常要查的库：**ESB（MySQL 8）** 和 **ERP（Oracle 或 SQL Server）**。
堡垒机 / RDP 体验差。给 helper 加两条常驻连接通道，让 LLM 像在 Navicat
对话框里那样发 SQL，helper 通过预先开好的连接直接跑，而不是每次都拼 `-u -p` 走 CLI。

## 设计要点
- **长连接**：每个 connector 在 helper 启动时 `sql.Open + Ping` 一次。
  启动连不上仅 `logger.Printf("warn: ...")`，**不 fatal** —— 其它动作（status / tail /
  exec / 上传文件）仍能用。运行中连接挂了 → 重连一次再重试，仍失败返回错误。
- **复用 `database/sql` + 纯 Go 驱动**（单 exe，无外部依赖）：
  - MySQL 8：`github.com/go-sql-driver/mysql`
  - SQL Server：`github.com/microsoft/go-mssqldb`
  - Oracle：`github.com/sijms/go-ora/v2`
- **工具注册**：两个工具都注册在 helper；`Tools()` **仅在配置正确时**返回对应条目。
  server 一行不动 —— 保持「工具全在 helper」原则。
- **脚本执行**：把 SQL 按 `;` 切（保留 `'…'` `"…"` `--…` `/*…*/` 中的分号）；
  按首词判定 SELECT / SHOW / DESC / EXPLAIN / WITH / VALUES → `Query`，
  其他 → `Exec`。每个语句独立 try，错误信息带语句序号。
- **返回结构**：
  ```json
  {
    "driver": "mysql",
    "statements": [
      {"index":0,"type":"query","columns":["id","name"],"column_types":["int","varchar"],"rows":[[1,"a"]],"row_count":1,"took_ms":12,"truncated":false},
      {"index":1,"type":"exec","affected_rows":3,"took_ms":4}
    ],
    "total_took_ms": 16,
    "truncated": false
  }
  ```
- **行大小保护**：单行 `[]any` JSON 后超过 `cfg.max_output_size` 一半即截断（与现有 `exec`
  一致）；`max_rows` 默认 1000、上限 5000，超出截断并标 `truncated:true`。
- **安全**：所有 SQL 全量进 `helper.log`（沿用现有 audit 通道）。**不做 SQL 黑名单**——
  Navicat 不做；交给上游 LLM / 流程守门。
- **不配置时的行为**：未配置 → 工具不出现；配置错误（host 写错 / 缺 password）→ 启动
  warn + 工具不出现。
- **凭据明文**：`config.json` 里 password 明文 —— 沿用现有 token 设计；加密另议。

## 配置文件新增（向后兼容）
`config.json` 加两个可选块；老客户不写就完全无感。
```json
{
  "esb": {
    "host": "10.0.0.5", "port": 3306,
    "user": "esb_ro", "password": "***",
    "database": "esb"
  },
  "erp": {
    "type": "oracle",                       // 仅 erp 用；"oracle" 或 "sqlserver"
    "host": "10.0.0.6", "port": 1521,
    "user": "erp_ro", "password": "***",
    "database": "ORCL"                      // oracle=service_name；sqlserver=database
  }
}
```
`Validate()` 不加新必填；DB 字段只在非 nil 时校验。

## 实施清单
- [ ] **Step 1 — config**：
  `internal/config/config.go` 新增 `DBConfig` 结构 + `ESB *DBConfig` / `ERP *DBConfig` 字段；
  `Validate()` 不加必填；非 nil 时调 `(*DBConfig).Validate()` 校验 host/port/user/password。
- [ ] **Step 2 — dbop 包**：
  新建 `internal/dbop/`
  - [ ] `dbop.go`：`Op` 接口、`Status` / `QueryResult` / `ExecResult` / `StatementResult`。
  - [ ] `sqlop.go`：基于 `*sql.DB` 的实现，带「运行出错 → 重连一次再试一次」策略，
        池 `SetMaxOpenConns(4)` / `SetMaxIdleConns(1)`。
  - [ ] `mysql.go` / `oracle.go` / `sqlserver.go`：DSN 构造。
  - [ ] `statement.go`：脚本拆分器（保留 `'…'` `"…"` `--…` 行注释 / `/* … */` 块注释
        中的 `;`）。
  - [ ] `fake.go`：`FakeOp` 让单测不依赖真实驱动。
  - [ ] `*_test.go`：拆分器 / Query-Exec 分流 / max_rows 截断 / reconnect 重试 / 不配置报错。
- [ ] **Step 3 — actions 接入**：
  `internal/actions/actions.go` 的 `Registry` 加 `dbops map[string]dbop.Op`；
  `Tools()` 改成动态生成（仅当对应 Op 非 nil 时出现）；新增两个 dispatch case
  （`esb_db_operator` / `erp_db_operator`），共享私有方法 `dbOperator`。
- [ ] **Step 4 — 启动注入**：
  `cmd/helper/main.go` 在 `actions.NewRegistry` 之前构造两个 `dbop.Op`
  （失败仅 `logger.Printf("warn: ...")`，不 fatal）；通过 `actions.WithDB` 注入。
- [ ] **Step 5 — Tool schema**：
  `internal/actions/tools.go` 新增 `dbOpSchema`：
  `{sql, max_rows?, timeout?, statement_index?}`。
- [ ] **Step 6 — 测试**（全部单测，无外部 DB 依赖）：
  - [ ] `statement_test.go`：单语句 / 多语句 / 含 `';'` 字符串 / 含 `--` 注释 / 含 `/* */`。
  - [ ] `sqlop_test.go`：Query/Exec 分流、max_rows 截断、reconnect 一次重试、Close 幂等。
  - [ ] `actions_test.go`：不配置时 `Tools()` 不含 dbop 工具；
        配置存在但 Op 为 nil 时 dispatch 返回 `"esb db not configured"`。
  - [ ] `dispatch_test.go`：补一条「不配置时 Tools() 长度正确」。
- [ ] **Step 7 — 文档 & 重打包**：
  - [ ] `docs/需求文档.md` 加 FR-8「数据库直连」段落；
  - [ ] `docs/设计文档.md` 加 §13「数据库工具」；
  - [ ] `config.example.json` 加 `esb`/`erp` 注释块；
  - [ ] `go mod tidy`；
  - [ ] `go build -ldflags="-s -w" -o bin\helper.exe .\cmd\helper`。
- [ ] **Step 8 — 自测**：
  本地用 MySQL 跑端到端（helper 上线 → `esb_db_operator` 发 SELECT → 看到行）。
  Oracle / SQL Server 在客户机上首次启用时再验证。

## 风险 / 取舍
- **驱动体积**：三个驱动加起来几 MB，仍远低于单 exe 目标。
- **Oracle 驱动**：`sijms/go-ora/v2` 是社区版（非 Oracle 官方）；老版本（10g/11g）
  可能有限制。客户若是新库（12c+）没问题，遇到再说。
- **多语句**：`database/sql` 不暴露 multi-statement；
  我们用客户端拆，对 `BEGIN … END;` PL/SQL 块不友好（v1 简单切分，不解析 PL/SQL）。
  够用，超出再说。
- **审计量**：每条 SQL 全量进 helper.log；后续量大可改 hash + 前 200 字符。
- **连接空闲超时**：MySQL 默认 `wait_timeout=8h`、SQL Server 默认无；
  helper 一直保持出站心跳，连接基本不会被动关。
  万一被关了，下次 Query 会自动重连（已包含在重试策略里）。

---

## 完成记录（2026-09-12）

- [x] Step 1 — `internal/config/config.go`：`DBConfig` + `ESB` / `ERP` 字段，
      `Validate()` 仅在非 nil 时校验，配置错误不阻止 helper 启动。
- [x] Step 2 — 新增 `internal/dbop/`：`Op` 接口、`SQLOp` 生产实现、
      `FakeOp` 测试替身、`SplitScript` / `RouteStatement` / `RunScript` 高层 helper、
      三种驱动的 DSN 构造器。驱动：`go-sql-driver/mysql`、`microsoft/go-mssqldb@1.8.2`、
      `sijms/go-ora/v2@2.5.0`。
- [x] Step 3 — `internal/actions/`：`Registry.dbops map[string]dbop.Op` 字段 +
      `WithDB(name, op)` 构造器；`Tools()` 动态过滤；`Dispatch` 加两个 dbop case。
- [x] Step 4 — `cmd/helper/main.go` 加 `wireDB()`：启动 ping 失败仅 warn，
      Op 仍注入（lazy retry 走 `SQLOp` 的重试逻辑）。
- [x] Step 5 — `internal/actions/tools.go`：`dbOpSchema` 共享 schema +
      `dbOpToolDefs` 元数据；工具仅在 `WithDB` 之后出现。
- [x] Step 6 — 测试覆盖：
      - `internal/dbop/dbop_test.go` 18 条：拆分器（含 `'…'`/`"` `` ` `` `--`/`/* */`/`$$…$$`）、
        路由、截断、不配置、空脚本、FakeOp 行为、`isRetryable` 真值表、DSN 子串断言、
        URL-encoded password。
      - `internal/actions/dbop_test.go` 7 条：未配置时不出现、配置后出现、`WithDB(nil)` 移除、
        未配置 dispatch 报错、缺 sql 字段、Query 端到端、max_rows clamp、多语句拆分。
      - `internal/config/config_test.go` 5 条：DBConfig 解析、默认端口、缺字段、坏 type、
        不配置 OK。
- [x] Step 7 — 文档与重打包：
  - `docs/需求文档.md` 加 FR-8「数据库直连」。
  - `docs/设计文档.md` 加 §13（模块边界 / 连接生命周期 / 脚本拆分 / Query-Exec 路由 /
    行编码 / 响应 JSON / 驱动与 DSN / 重试 / 测试）。
  - `config.example.json` 加 `esb`/`erp` 块（占位符值）。
  - `bin\helper.exe`（13.6 MB）+ `bin\server.exe`（7.8 MB）已重建。
- [x] Step 8 — `go vet ./...` + `go test ./...` 全绿。

实际驱动体积比 plan 估算的「几 MB」大（mysql 2 MB / sqlserver ~6 MB / oracle ~5 MB，
共 ~13 MB 是因为 `go-ora/v2@2.5` 还依赖 `go-ora@v1` 的几个子包）。考虑过用旧版
（denisenkom / go-ora@2.1.15）砍掉 Azure/gorm 间接依赖、但 13.6 MB 单 exe 仍在
预算内，未做更激进的瘦身。

## 端到端冒烟（待办）

- [ ] 用真实 MySQL 跑一次：helper 启动 → `esb_db_operator` 发 `SELECT 1` → 看到行。
- [ ] 真实 Oracle / SQL Server 在客户机上首次启用时跑一次。
