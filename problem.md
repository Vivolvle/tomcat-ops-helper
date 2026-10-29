# problem.md — 使用问题登记（MCP `report_problem` 自动写入）

其他项目的 agent 在使用 tomcat-helper 的 MCP 工具过程中遇到任何不方便、缺失能力或
报错，都应通过 `report_problem` 登记，而不是默默绕过。operator 会定期处理这里的内容。

<!-- 新条目由 server.exe 追加在文件末尾；上面的说明不要删。 -->

（暂无未处理条目。）
## [2026-10-22 14:36:41]
- 来源: yc-domains Nantong production deployment
- 工具: upload / exec
- 问题: 2026-10-22 14:34左右部署南通两个jar。upload_guide指引使用127.0.0.1:7777/upload上传core(1090385字节)时返回 upload chunk failed: helper disconnected before result；随后 exec/esb_db_operator 均 unknown client_id("nantong")，HTTP /clients 返回空数组。尚未停服或替换线上jar。
- 期望: 上传失败应保留helper连接，重连后应可核验上传状态。

## [2026-10-29 10:51:58]
- 来源: yc-domains
- 工具: upload
- 问题: 大文件 29MB 上传出现空响应；并发 1MB 分片返回 502/404，单次分片上传有 result=null 且目标仅零字节 .tmp；单个 1MB jar 可完成。无法确认完整部署包传输成功。
- 期望: 上传应返回明确成功及远端校验结果，超时或失败给出错误。

