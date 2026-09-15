# tomcat-ops-helper

一个跨公网远程运维 Windows Tomcat 客户机的小工具集。

## 架构

```
[客户机]                       [operator 本机]                      [opencode agent]
helper.exe  ──reverse-wss──▶  cloudflared tunnel  ◀──HTTPS──       mcpstdio.exe
 (wss 客户端)                  server.exe (wss relay + HTTP bridge)  (stdio→HTTP)
                              监听 :8443 (wss) / :8444 (ws)         监听 127.0.0.1:7777
                              HTTP bridge :7777                      pinned MCP_CLIENT_ID
```

`server.exe` 一个进程可以同时接多个 `helper.exe`（不同 `client_id`），
每个 opencode workspace 用 `MCP_CLIENT_ID` 钉到自己的客户机。

## 目录

```
cmd/helper/    customer-side reverse-wss 客户端
cmd/server/    wss 中继 + HTTP bridge
cmd/mcpstdio/  stdio→HTTP 桥（MCP Inspector 用）
cmd/gencert/   自签证书生成器
internal/      protocol / config / execshell / actions / wsclient / wsserver
examples/      各项目的 config.json + opencode.json 样例
bin/           编译产物（helper.exe / server.exe）
certs/         TLS 证书（首次启动前生成）
```

## 一次性准备

```powershell
# 1. 自签证书（仅本地测试用，生产换正式证书）
.\gen-cert.ps1

# 2. 编译
go build -ldflags="-s -w" -o bin\helper.exe .\cmd\helper
go build -ldflags="-s -w" -o bin\server.exe .\cmd\server
go build -ldflags="-s -w" -o bin\mcpstdio.exe .\cmd\mcpstdio
```

## 启动 operator

两个独立终端，分别跑：

**终端 1 — server.exe**

```powershell
.\run-server.ps1
```

看到 `wss listening on :8443` + `agent http listening on 127.0.0.1:7777` 即就绪。

**终端 2 — cloudflared 内网穿透**

`cloudflared` 全路径是 `D:\tools\cloudflared.exe`，powershell 里直接敲全路径即可。

两种用法二选一：

```powershell
# A. 快速隧道（URL 随机，每次启动都变；适合测试）
D:\tools\cloudflared.exe tunnel --url http://localhost:8444
```
输出里有形如 `https://xxxx.trycloudflare.com` 的 URL，把它配到客户机
`config.json` 的 `server_url`（路径加 `/ws`，例：
`wss://xxxx.trycloudflare.com/ws`）。

```powershell
# B. 命名隧道（固定域名；生产用，需要先在 cloudflare 配 tunnel）
D:\tools\cloudflared.exe tunnel login
D:\tools\cloudflared.exe tunnel create helper-ops
D:\tools\cloudflared.exe tunnel route dns helper-ops helper.example.com
D:\tools\cloudflared.exe tunnel run helper-ops
```

注意：

- `--url` 接的是 `ws_listen` 端口（默认 `127.0.0.1:8444`，**plain WS**），
  cloudflared 不接受 `wss://` 作为 origin scheme。
- `wss://` 端口（`:8443`）是给直连或自有反代用的，不走 cloudflared。

## 配置 server.json

```json
{
  "listen": ":8443",
  "ws_listen": "127.0.0.1:8444",
  "tls_cert": "D:\\workspace\\ai_workspace\\tomcat-ops-helper\\certs\\server.pem",
  "tls_key":  "D:\\workspace\\ai_workspace\\tomcat-ops-helper\\certs\\server.key",
  "tokens": ["token-A", "token-B"],
  "audit_log": "audit.log",
  "agent_listen": "127.0.0.1:7777"
}
```

**加新客户**：往 `tokens[]` 里 append 一条新 token，然后**重启
server.exe**（tokens 是启动时一次性读入）。已有 helper 不受影响。

## 配置客户机 helper

把 `bin\helper.exe` 跟 `config.json` 一起放到客户机（例如
`D:\bs-esb\helper-client\`），启动：

```powershell
.\run-helper.ps1
```

`config.json` 关键字段：

```json
{
  "server_url":  "wss://helper.example.com/ws",
  "token":       "token-A",
  "client_id":   "shanghai",
  "tomcat_home": "D:\\tomcat",
  "jdk_home":    "D:\\jdk",
  "log_dir":     "D:\\tomcat\\logs",
  "allowed_dirs":["D:\\tomcat","D:\\tomcat\\logs","D:\\workspace"],
  "esb": { "host":"10.0.0.10","port":3306,"user":"esb_app","password":"...","database":"esb" },
  "erp": { "type":"oracle","host":"10.0.0.20","port":1521,"user":"erp_app","password":"...","database":"orcl" },
  "insecure_skip_verify": true
}
```

- `client_id` 全局唯一，两个客户不能重名；不填回退到
  `helper-<hostname>`。
- `esb` 默认 MySQL（`type` 留空），`erp.type` 写 `"oracle"` 或
  `"sqlserver"`。两个 section 都可选——不配就不注册对应 DB 工具。
- `allowed_dirs` 是 `exec.work_dir` 的白名单，配少了 `exec` 会
  返回 `work dir not allowed`。

完整模板见 `examples/shanghai/config.json`。

## opencode agent 配置

operator 本机的每个项目 workspace 里放一份
`.opencode/opencode.json`：

```json
{
  "mcp": {
    "tomcat-helper": {
      "type": "local",
      "command": ["D:\\workspace\\ai_workspace\\tomcat-ops-helper\\bin\\mcpstdio.exe"],
      "enabled": true,
      "environment": {
        "MCP_HTTP_URL":   "http://127.0.0.1:7777",
        "MCP_CLIENT_ID":  "shanghai"
      }
    }
  }
}
```

- `MCP_HTTP_URL`：所有 agent 共用同一个 `127.0.0.1:7777`。
- `MCP_CLIENT_ID`：钉到这台机器对应的 helper。同一台 operator 上跑
  N 个 opencode workspace（N 个项目）就这么并排。
- 只连了一个 helper 时 `MCP_CLIENT_ID` 可省略，多个必填。

完整模板见 `examples/shanghai/opencode.json`。

## 多客户并行（一张表）

| 资源 | 数量 | 说明 |
|------|------|------|
| server.exe | 1 | 一个进程接所有 helper |
| cloudflared | 1 | 一个域名/URL 暴露 :8444 |
| tokens[] | N | 每个客户一条 |
| helper.exe | N（每台客户机一个） | `client_id` 唯一 |
| mcpstdio.exe | N（每个 opencode agent 一个） | `MCP_CLIENT_ID` 唯一 |

## 常用操作

### 健康检查

```powershell
curl.exe -k http://127.0.0.1:7777/health
curl.exe -k http://127.0.0.1:7777/clients
```

### HTTP bridge 调命令

```powershell
curl.exe -k -X POST http://127.0.0.1:7777/command `
  -d '{"action":"status"}'

curl.exe -k -X POST http://127.0.0.1:7777/command `
  -d '{"action":"exec","params":{"cmd":"dir D:\\workspace","work_dir":"D:\\workspace"}}'
```

### MCP 风格

```powershell
curl.exe -k http://127.0.0.1:7777/mcp/tools?client_id=shanghai

curl.exe -k -X POST -H "Content-Type: application/json" `
  http://127.0.0.1:7777/mcp/call `
  -d '{"jsonrpc":"2.0","id":"1","method":"tools/call",
       "params":{"name":"status","client_id":"shanghai"}}'
```

### 上传文件到客户机（jar / war / 新二进制）

```powershell
$sha = (Get-FileHash .\bin\helper.exe -Algorithm SHA256).Hash.ToLower()

curl.exe -k -X POST `
  -F "file=@.\bin\helper.exe" `
  -F "path=D:\bs-esb\helper-client\helper.exe" `
  -F "sha256=$sha" `
  -F "overwrite=true" `
  http://127.0.0.1:7777/upload
```

服务端把文件切成 64 KB 一片，走 `upload_file` 命令推到 helper，
最后一片做 sha256 校验再原子改名。

## 部署新版本 helper.exe

```powershell
go build -ldflags="-s -w" -o bin\helper.exe .\cmd\helper
Get-FileHash bin\helper.exe -Algorithm SHA256

# 通过 /upload 推到客户机（见上）

# 客户机重启
taskkill /F /IM helper.exe
.\run-helper.ps1
```

无 console 版（隐藏窗口）：

```powershell
go build -ldflags="-H windowsgui -s -w" -o bin\helper.exe .\cmd\helper
```

## 测试 / lint

```powershell
go test -count=1 -timeout 60s ./...
go vet ./...
```

## 常见问题

- **`unknown token`**：helper 的 token 不在 `server.json.tokens[]` 里，
  或者 server.exe 启动后追加了 token 没重启。
- **`work dir not allowed`**：`exec.work_dir` 不在 `allowed_dirs` 白名单。
- **`action is required` / `bad request`**：`bin\server.exe` 是旧的，
  重编一次。
- **helper 连不上**：cloudflared 没起 / `server_url` 路径没带 `/ws` /
  证书 `insecure_skip_verify` 没开。
- **GOPROXY 被墙**：`go env -w GOPROXY=https://goproxy.cn,https://proxy.golang.org,direct`。
