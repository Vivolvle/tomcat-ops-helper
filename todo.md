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

---

# 实战痛点 & 改进项（2026-09-14 客户机首次端到端）

按"是否要改本项目代码"分类。一次性记下来，避免每次踩同样的坑。

## A. 真 bug —— 本项目要改

- [x] **A1 — `buildDSN` 用 `url.QueryEscape` 编密码，导致 mysql_native_password 1045**
  现象：helper 启动时 ping MySQL 报 `Access denied (using password: YES)`，但手工
  `mysql -uroot -pyc1234!` 同一密码能连。`yc1234!` 里 `!` 被 `QueryEscape` 编成 `%21`，
  整个 7 字节 `yc1234!` 变成 8 字节字面值 `yc1234%21`，server 哈希对不上。
  fix：新增 `mysqlDSNEscape`，只 escape DSN userinfo 分隔符（`@` 和 `:`），其它字符原样
  透传。`mysql` / `oracle` 两个分支都改；`sqlserver` 走 `url.Values`，不受影响。
  dbop_test.go 加 `TestSQLOp_DSNPreservesExclamation` 回归。**已修**，helper.exe 已重打。

- [ ] **A2 — dbop_test.go 原来那条 `p%3Bw%40rd…` 断言本身就是错的**
  发现 A1 的过程里看到：`TestSQLOp_EnsureOpenCarriesCredentials` 期望 password 用
  `url.QueryEscape` 编码（line 440 那行 `wantPass := url.QueryEscape(c.cfg.Password)`）。
  这是当时实现自带回归测试，不是验 DSN 格式、而是验"必须有 escape"。意味着
  QueryEscape 这个错误选型**从一开始就没被测到**。值得回查 PR/commit，看是啥时候
  引入的；同时把 `TestSQLOp_DSNPreservesExclamation` 的 `wantPass` 写法跟 `mysqlDSNEscape`
  绑死，以后改 escape 策略测试会跟着红。

## B. 实操体验 —— 本项目要改 / 增强

- [ ] **B1 — `validate_config` action**
  痛点：改完 config.json 后，helper 不监听文件变更、不重读，只能看启动日志里的
  warn 行才能知道连不连得上。期望：加一个 `validate_config` action，**不重连 DB**，
  只回显当前 helper 进程实际读到的 esb/erp 配置（host/port/user/database，password 脱敏为
  `***`），并对每个 connector 跑一次 ping。LLM 端：改 config → 重启 helper → 调
  `validate_config` → 看 ping 结果。这样改错密码的反馈从"重启后翻日志"变成"一次工具调用"。

- [ ] **B3 — DSN 参数文档化**
  现状：MySQL DSN 只有 `parseTime=true&charset=utf8mb4&timeout=10s&readTimeout=30s&writeTimeout=30s`。
  MySQL 8 默认 `caching_sha2_password`，用户若是这条 + 老 go driver 版本会握手失败。
  当前我们用 `mysql_native_password` 所以 OK。期望：docs/设计文档.md §13 加一段
  说明这个权衡，并给出"客户端需要 native password 时"的 workaround（DB 侧
  `ALTER USER … IDENTIFIED WITH mysql_native_password BY '…'`）。**纯文档**。

- [ ] **B4 — `kill_zombie` 工具在改完 helper 后做端到端测试**
  本次没用到，但 status / kill_zombie / replace_jar / regen_service / restart 这些
  工具都没在这次实战中走过。需要排个简单的脚本走一遍。

## C. exec 工具的封装坑 —— 本项目要改

- [ ] **C1 — exec 对 cmd /C + 路径含空格的 tokenize 不稳**
  实战中：`cmd /C "if not exist \"D:\\MySql\\MySQL Server 8.0\\Data\""` 直接被 exec
  当作字面命令跑（引号丢），work_dir 还附加到前面。当前 exec 用 `cmd /C <cmd>`，
  cmd 自己再 tokenize 一次。期望：exec 在收到命令时，如果第一个 token 是 cmd /C / powershell /
  pwsh 之类，自己做一层 escape 或者干脆在 helper 里**先 `powershell -NoProfile -Command "<user-cmd>"`**
  把所有用户命令都包成 PS -Command（PS 对引号处理更稳）。也可以加个 flag：
  `cmd: "powershell"` 让 exec 走 powershell 路径，绕开 cmd tokenize。

- [ ] **C2 — exec 不接受 shell 重定向 / 管道**
  `command 2>&1` 被 exec 当成命令的一部分传给 mysql 当 db 名（mysql 报 1049 unknown
  database）。当前 exec 是 `cmd /C <cmd>` 但 cmd 重定向已经被吃。期望：文档化
  "exec 不支持 shell 重定向 / 管道 / &"；如果用户需要，改用 powershell -File
  跑自定义 ps1 脚本（这是 AGENTS.md 的推荐做法）。在 tools.go 给 `exec` tool
  description 加一段说明。

- [ ] **C3 — PowerShell `$host` 等保留变量在用户 ps1 里**
  不是我代码问题，但既然实战踩了，AGENTS.md 加一行：
  「PowerShell 单引号字符串里 `\` 是字面字符，`\\` 才是两个字符」+「避免用 `$host`
  / `$args` / `$input` / `$matches` / `$PSItem` / `$null` 等保留名做变量」。

## D. 流程 / 文档 —— 本项目 AGENTS.md / docs 要补

- [ ] **D1 — helper.exe 部署 checklist**
  实战里每次改完 helper 源码都要：
  1. 本地 `go test ./...`
  2. 本地 `go build -ldflags="-s -w" -o bin\helper.exe .\cmd\helper`
  3. 复制到 `D:\tools\helper-client\helper.exe`（操作员本机）
  4. 把新版拷到客户机 `D:\bs-esb\helper-client\helper.exe`（覆盖旧版）
  5. 停旧 helper（taskkill / sc），启动新 helper（run-helper.ps1）
  6. 看启动日志有没有 "esb_db_operator connected" / "erp_db_operator connected"
  7. （可选）调 `validate_config` ping 一次

  AGENTS.md 加一个 "Deploying a new helper.exe" 段，包含 sha256 校验方法。
  当前版本号 helper.log 里有 `helper 0.1.0 starting`，可以借此核对新旧。

- [ ] **D2 — 把"datadir 迁移 / my.ini 改 / 服务启停"这类实战脚本沉淀到 D:\bs-esb\helper-client\**
  实战里我写了 `robocopy_data.ps1` / `patch_myini.ps1` / `patch_config.ps1` /
  `patch_erp.ps1` 等等。helper-client 目录可以有一个 `scripts/` 子目录放这些，
  命名清晰（`01-stop-mysql.ps1` / `02-robocopy-data.ps1` / ...）。下次同类活
  直接复用。**纯组织工作**。

- [ ] **D3 — todo.md 自身的清洁**
  现在 todo.md 已经有两份"完成"的 plan（FR-8 dbop + upload_file），实战痛点
  是新一类。考虑分文件：`todo.md`（MCP gap）、`docs/实战日志.md`（本次及以后
  实战端到端记录）、`docs/改进 backlog.md`（A/B/C/D 这批）。

## 优先级建议（自我进化这一轮）

只做 **A2** + **B1** + **C1** + **D1**：
- A2 — 一行测试断言的事，防止回归
- B1 — 加 `validate_config` action，下次改密码不再盲等重启
- C1 — 给 exec 加 powershell 路径，避免 cmd tokenize 翻车
- D1 — 文档化部署流程，下次不靠脑子

其余留 backlog，下次实战 / 复盘再决定。

---

# 2026-09-14 第二次实战补丁

## E. 上次自我进化漏掉的回归坑（已修）

- [x] **E1 — execSchema 的 description 里有未转义的 `"`，整个 ToolList 变非法 JSON**
  现象：helper 启动后 esb_db_operator / erp_db_operator 都连上（说明 DB 端 OK），
  但 wsclient PostConnect 写 ToolList 时 server 端报 `invalid character 'S' after
  object key:value pair`，每 5s 重连一次。
  根因：上一轮我给 `execSchema` 的 `shell` 字段 description 里写了
  ``"…wraps the command in `powershell … \"Set-Location '<work_dir>'; <cmd>\"` …"`` —
  反引号 raw string 里 `\"` 是两个字面字符（反斜杠+引号），**JSON 解析时那对 `"`
  提前结束了 description string**，剩下的 `\` 当成 JSON 顶层 token，'S' 是 `Set-` 的 'S'。
  fix：把 description 里的伪 inline 例子换成语义化文字，去掉嵌入反引号 / 嵌套引号。
  顺手加了 `TestTools_SchemasAreValidJSON` —— 跑 `json.Unmarshal` 验证每个 Tool
  的 `inputSchema` 都是合法 JSON 对象，**任何 schema 出错都立刻红**。

  **反思**：之前"全量 test + vet"没抓到这个，因为我的断言都是行为级，没验
  schema 文本本身的合法性。**新增静态校验**。

- [ ] **E2 — 自我进化完没真跑一遍 "Tools() 全列出来" 的端到端验**
  本次问题在 PostConnect 阶段才会暴露（连 DB 之前）。我没有"helper 启动 →
  立即通过 MCP 调 `tools/list`"的本地冒烟测试，导致第一次实战就翻车。
  期望：在 `cmd/helper/main.go` 加一个 `cmd/helper/dump_tools.go` 子命令（不是
  helper 正常运行时用的，只是开发期工具）`go run ./cmd/helper dump-tools` 把
  Tools() 列表 + 每个 schema 渲染到 stdout，方便人眼 review。
  也可以在 `dispatch_test.go` 里加一条 TestPostConnectToolList_IsValidJSON
  端到端跑 PostConnect，断言 wsserver 收到的 envelope.ToolMeta[i].InputSchema
  都能 Unmarshal。

## F. 这次实战还没修、但可能相关的

- [ ] **F1 — 服务器端 Hello 接收 vs PostConnect 顺序**
  当前 helper 是"先发 Hello → 然后 PostConnect 里发 ToolList"。如果 server 期望
  收到 ToolList 之前先 ACK Hello，而 helper 在 ACK 之前就把 ToolList 写出去，
  server 可能因为 protocol race 把 ToolList 当成下一帧的某个 message type 报错。
  这条需要看 server 的协议处理代码确认，不能瞎猜。

## 增量 todo（保持简洁，跟踪就行）

- E1 ✅ 已修 + 防回归
- E2 写个 dump-tools 子命令 / TestPostConnectToolList_IsValidJSON
- F1 看 server 协议处理

---

# 2026-09-15 第三次实战（客户机全装 MySQL 8.0.45，全程只走 helper）

场景：zip 解压 → 初始化 datadir → 注册 MYSQL80 服务 → 改 root/yc1234! + native password →
防火墙放行 → 清理临时脚本。`write_file` / `upload_file` 都很好用；问题基本集中在
**exec 通道**和 **DB connector 生命周期**两块。

> **2026-09-15 修复轮**：G1 / G2 / G3 / G4①③ 已修（回归测试全过）；
> 遗留 open：G4② `reload_config`、H1 `read_file`、H2 `list_logs` 列目录、I1 `run_script`。

## G. exec / 命令通道

- [ ] **G4② — reload_config：不重启 helper 重读 config.json / 重建 DB connector / 刷新 Tools()**
  需动 helper 主流程 + wsclient 重发 ToolList，留待下一轮。

## H. 文件读写

- [ ] **H1 — 没有 `read_file`，只有 `tail`**
  本次要读 `config.json` / `README.md` / `helper.log`（都在 allowed_dirs 内），只能
  `tail -n 200` 兜：不能从头读、不能按行区间取，对非日志文件语义别扭。
  期望：`read_file{path, offset?, limit?}`，白名单校验 + 字节上限 + 超限截断标记
  （复用 write_file 的路径检查即可）。

- [ ] **H2 — `list_logs` 不列子目录**
  现象：列 `E:\bs-esb` 时只返回文件，`helper-client\`（目录）不出现；`dir /b` 可见。
  期望：目录项也返回（`type:"dir"` + mtime），或加语义更准的 `list_dir`。
  （`list_logs` 这名字会误导，我一开始以为它只服务 log_dir。）

## I. 锦上添花（可选）

- [ ] **I1 — exec 增加脚本模式 / `run_script`（C2 延伸）**
  本轮几乎每条复杂命令都要"write_file 落 .cmd → exec 跑"两步；cmd 不支持 `;`/`&`/`|`/`>`
  没法一句话办多事，还踩过 `del a; dir b` 被当成单个 del、`/b` 变成 del 的非法开关
  （`Invalid switch - "b"`）这种歧义坑。
  期望：exec 收到多行 `script` 参数时自动写临时脚本→执行→回读输出→清理；或独立
  `run_script` action。仍受 allowed_dirs + 超时 + 输出截断约束。


