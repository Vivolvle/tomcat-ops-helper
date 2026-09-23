# http_call：不依赖 curl 的 HTTP 调用

**可以直接在 headers 参数中传 token**，包括 Authorization、X-Access-Token、Cookie、X-Sign。
helper 原样发送这些请求头，不要求先写文件，也没有凭证文件路径确认步骤。
headers_file / token_file 只是可选方式，不是使用本工具的前置条件。

helper 使用 Go 原生 HTTP 客户端，在客户机发起请求，不需要安装 curl。
`localhost` 指客户机。server 只负责转发，升级 helper 并重启后自动公布新工具。

curl 参数映射：`--url` → `url`，`-X` → `method`，每个 `-H` → `headers` 中的一项，
`--data-raw` → `body`。curl 带 data 默认 POST；调用此工具时应明确填写 POST。
工具接收结构化参数，不解析整条 curl 命令。

```json
{
  "name": "http_call",
  "arguments": {
    "method": "POST",
    "url": "http://localhost:8060/server/esb/event/retryOne?dsCode=erp&consumerCode=SF_SCC_WAYBILL_CREATE&eventId=2",
    "headers": {
      "Accept": "application/json, text/plain, */*",
      "Content-Type": "application/json;charset=UTF-8",
      "Authorization": "<token>",
      "X-Access-Token": "<token>",
      "X-Sign": "<当前请求签名>",
      "X-TIMESTAMP": "<当前时间戳>",
      "X-Tenant-Id": "1000",
      "X-Version": "v3"
    },
    "body": {},
    "timeout": 30
  }
}
```

其他请求头（Origin、Referer 等）也可按需加入 headers。工具不生成业务签名或刷新 token，
调用方应提供接口需要的有效请求头。body 为对象时发送 JSON，并默认添加 application/json；
body 为字符串时原样发送，可以发送 YAML、XML 或表单文本，并自行指定 Content-Type。
method 默认 GET，timeout 默认 30 秒，范围 1–300 秒；桥接调用的外层 timeout 也须足够长。

## 可选：凭证不经过工具参数

在 helper 的 allowed_dirs 内准备 JSON 文件，内容为请求头字典，例如：

```json
{"Authorization":"<token>","X-Access-Token":"<token>"}
```

传 `headers_file: "D:\\bs-esb\\helper-client\\http-headers.json"`，helper 在本机读取并注入，
文件请求头覆盖同名 headers。调用方无需 read_file 读取凭证，也不必把凭证放进 transcript。
如果文件是 `{"token":"..."}`，可改用 `token_file`、`token_field: "token"`、
`token_header: "Authorization"`、`token_prefix: ""`。
token_prefix 默认 `Bearer `，本项目示例中的 Authorization 无 Bearer，须显式设为空。
嵌套 token 支持点分字段路径，例如 data.access_token。文件限 64 KiB，检查符号链接解析后的路径。

## 响应与边界

### 排查参数和连通性

没有业务 header 名白名单，也没有 helper 自定义 header 数量限制。名称必须符合 HTTP token
语法，值必须是字符串且不能含 CR/LF 等控制字符。例如 `"X-TIMESTAMP":"1790144860313"`、
`"X-Tenant-Id":"1000"`，不要使用 JSON 数字。远端服务仍可能限制请求头总大小。
参数错误会指出字段名；不会把错误字段中的凭证值打印出来。

- `debug:true`：随结果返回请求方法、脱敏 URL、请求头名称、body 字节数、超时设置。
  所有请求头值及 URL 查询参数值均隐藏，正文不进入 debug；不是可直接执行的 curl 命令。
- `dry_run:true`：只校验及组装，返回 `sent:false` 和 debug，不发网络请求。
  这只能检查参数，不能证明网络或业务鉴权成功。
- `url:"helper://echo"`：临时启动随机端口的 127.0.0.1 回显服务，经过实际 HTTP 客户端调用，
  返回收到的方法、请求头和 body。已知凭证回显脱敏。结束后关闭服务，无需配置端口。
  这证明 helper 本机 HTTP 发送正常，不代表 ESB 目标可达或鉴权通过。

```json
{"url":"helper://echo","method":"POST","headers":{"X-TIMESTAMP":"1790144860313","X-Tenant-Id":"1000","Authorization":"test-only-token"},"body":{},"debug":true}
```

返回 status_code、ok、headers、body（字符串）、truncated、took_ms。
4xx/5xx 保留状态码和响应体，ok=false；业务 JSON 的成功状态仍需由调用方判断。
默认响应体最多 64 KiB，可用 max_response_bytes 调整，最大 1 MiB。
读响应中断会返回 read_error，写请求可能已生效，不应盲目重试。

不自动重定向、不自动重试、不忽略 HTTPS 证书校验。URL 必须显式指定 HTTP(S) 地址，
不会推断生产/开发环境或端口。不会自动执行 apply/retry 等后续业务操作。
已知凭证在响应头/正文中的原文、JSON 转义和 URL 编码回显会脱敏；不保证识别任意变换后的秘密。
直接通过 headers 传入的凭证仍会出现在调用参数中；需要避免这一点时使用文件方式。
