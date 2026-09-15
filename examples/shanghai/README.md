# 上海客户 — 接入示例

放到客户机上的 `D:\bs-esb\helper-client\config.json`，把示例里所有
`CHANGE-ME` 替换成实际值：

- `server_url`：operator 的隧道入口，正式环境是固定域名（例
  `wss://helper.example.com/ws`），临时调试用 cloudflared quick tunnel
  的随机 `https://<random>.trycloudflare.com`。
- `token`：必须出现在 operator 端 `server.json` 的 `tokens[]` 数组里，
  建议每个客户一条。
- `client_id`：唯一标识，operator 的 opencode agent 通过
  `MCP_CLIENT_ID` 找到这台机器。两个客户不能重复。
- `tomcat_home` / `jdk_home` / `log_dir` / `allowed_dirs`：按客户机
  实际情况填，`allowed_dirs` 是 `exec.work_dir` 的白名单。
- `esb` / `erp`：DB 连接器。ESB 默认 MySQL，`type` 留空；ERP
  `type` 写 `"oracle"` 或 `"sqlserver"`。两个 section 都是可选——
  不配就不注册 `esb_db_operator` / `erp_db_operator` 工具。

`opencode.json` 放在 operator 这边、对应项目 workspace 的
`.opencode/opencode.json`。同一台 operator 上跑 n 个 opencode agent，
各自指向同一个 `http://127.0.0.1:7777`，靠 `MCP_CLIENT_ID` 区分。
