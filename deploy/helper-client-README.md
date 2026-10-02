# helper-client

Customer-side bundle for the tomcat-ops-helper reverse-wss helper. Copy the
whole folder onto the target Windows machine.

## 推荐方式:双击 `helper-gui.exe`(可视化配置)

浏览器会自动打开配置界面(没弹出的话,看 helper-gui 窗口里打印的地址手动打开)。
界面里可以:

- **连接** — 填服务器地址和令牌,点「测试连接」当场验证;
- **路径 / 数据库** — 文件夹用「浏览…」从驱动器逐级选;数据库填好后点「测试连接」;
- **保存配置** — 自动校验并写回 `config.json`(不会破坏文件里的其他内容);
- **启动 helper / 停止 helper** — 启动后「日志」页签实时滚动运行日志;
- 服务器地址 / 令牌 / 心跳类修改保存后需**重启 helper** 生效(界面会提示)。

注意:**关闭 helper-gui 窗口会同时停止它启动的 helper**。需要长期后台运行时,
改用下面的 run-helper.bat 启动(此时 GUI 里会显示"外部实例",停止按钮不可用,
日志页自动切换为显示 helper.log 审计日志)。

## 手动方式(备用)

直接编辑 `config.json` 后用 run-helper.bat 启动 —— 以下为字段说明。

## Files

| File | Purpose |
| --- | --- |
| `helper-gui.exe` | 可视化配置界面(推荐入口)。 |
| `helper.exe` | The helper process. Built with console so logs stream to the terminal. |
| `config.json` | Per-deployment config — edit before running. |
| `run-helper.ps1` | PowerShell launcher. |
| `run-helper.bat` | cmd launcher. |

## Before first run

Edit `config.json` and replace every `REPLACE_ME`:

1. **`server_url`** — the `wss://xxxx.trycloudflare.com/ws` URL the operator gets
   when running on their box:
   ```powershell
   cloudflared tunnel --url wss://localhost:8444
   ```
   Paste it as `wss://xxxx-yyyy.trycloudflare.com/ws` — **the trailing `/ws` is
   required**; the server rejects (404) any WebSocket upgrade on other paths.
2. **`token`** — must match one of the tokens in the operator's `server.json`.
3. **`client_id`** — a short, human-friendly identifier for this helper (e.g.
   `benji`, `erp-prod-01`). The server uses it to route commands when more than
   one helper is connected, and to key the cached `ToolList` per helper. This
   is the id you'll see in `GET /clients` and pass back as the `client_id`
   parameter on `POST /command` / `POST /mcp/call` to disambiguate. Must be
   unique per machine on the same operator. Defaults to `hostname` if omitted.
4. **`tomcat_home` / `jdk_home` / `log_dir`** — paths on **this** (target) machine.
4. **`allowed_dirs`** — paths on this machine that the helper is allowed to write
   to. `exec` and `write_file` refuse anything outside this list.
5. **`esb` / `erp`** (optional) — DB connection blocks. When filled in, the
   helper advertises `esb_db_operator` and/or `erp_db_operator` tools; delete
   the whole block to hide a tool. `erp.type` is `"oracle"` (port 1521) or
   `"sqlserver"` (port 1433). Connection failure at startup only logs a warning
   — other tools keep working.

If you haven't generated a token yet, the operator side:
```powershell
# in the operator's working dir (D:\workspace\ai_workspace\tomcat-ops-helper)
$tok = -join ((48..57)+(65..90)+(97..122) | Get-Random -Count 32 | ForEach-Object {[char]$_})
# paste $tok into server.json tokens[] and into this helper's config.json token
```

## Run

Double-click `run-helper.bat` (or `run-helper.ps1`). A console window opens
showing live logs. To stop, close the window or `Ctrl+C` in PowerShell.

Logs also go to `helper.log` next to the executable.

## Stealth build (optional)

This bundle ships a console-mode helper for visibility. For a no-window
variant, rebuild on the operator's box with:
```powershell
go build -ldflags="-H windowsgui -s -w" -o D:\tools\helper-client\helper.exe .\cmd\helper
```
Logs still go to `helper.log`, just no terminal window.

## Verifying the tunnel end-to-end

Operator side:
```powershell
.\server.exe                                   # one terminal
cloudflared tunnel --url wss://localhost:8443   # another terminal, copy the https URL
```

Helper side (this bundle):
```powershell
# after editing config.json with the trycloudflare URL + matching token
.\run-helper.ps1
```

Operator sanity check (helper should appear within ~5s of the helper starting):
```powershell
curl http://127.0.0.1:7777/health
curl http://127.0.0.1:7777/clients
```
