# problem.md — 使用问题登记（MCP `report_problem` 自动写入）

其他项目的 agent 在使用 tomcat-helper 的 MCP 工具过程中遇到任何不方便、缺失能力或
报错，都应通过 `report_problem` 登记，而不是默默绕过。operator 会定期处理这里的内容。

<!-- 新条目由 server.exe 追加在文件末尾；上面的说明不要删。 -->

## [2026-09-15 20:03:23] — 已处理
- 来源: user-feedback
- 工具: tomcat-helper_exec
- 原始反馈：
  1. service.bat install 需要同时设置 JAVA_HOME 和 CATALINA_HOME 环境变量才能工作
  2. service.bat 不支持 start 参数
  3. tomcat10w.exe 需要重命名为 <服务名>w.exe
  4. config.json 修改后不支持热重载     ← **已剔除**（留作 backlog，不在 problem 范围）
  5. tomcat-helper_status 显示的 tomcat_home 似乎写死为 C:\tomcat
  6. 缺少验证 config.json 配置是否生效的工具

- 处置（2026-09-15）：
  - **①② 已修** —— `buildWindowsService` 现在同时 export `JAVA_HOME` + `CATALINA_HOME`，
    service.bat 新增 `start` case（默认走 `net start <ServiceName>`），保持与 Linux
    `service.sh start|stop|restart` 同语义。
  - **③ 已修** —— `regen_service` 检测到 bin\tomcat10w.exe 时自动复制为
    `<ServiceName>w.exe`（服务名来自 config.json 的 `service_name`，缺省时为
    `Tomcat<端口>`），并把操作结果回写到响应里。
  - **④ 已剔除** —— 热重载 config.json 是大改（要刷新 cfg 指针 / 重建 DB connector /
    重发 ToolList），单独放到 `todo.md` 的 backlog，不算 problem。
  - **⑤ 非 bug** —— `status` 返回的 `tomcat_home` 来自 `r.cfg.TomcatHome`，正是
    config.json 当前值；`tomcat_exists` 字段反映磁盘上该路径是否存在。如果看起来
    "写死 C:\tomcat"，几乎肯定是 config.json 修改后没重启 helper（helper 只在启动时
    读一次 config.json；这一点 ④ 已剔除，故保持现状）。新增 `status.config_loaded_at`
    字段，告警看起来值不对时直接拿这个时间戳对照 helper.log 里的 startup 日志。
  - **⑥ 非 bug** —— `validate_config` action 早就实现了，但同样依赖重启（与 ④ 同因）。
    文档（AGENTS.md + status 响应）已说明用法："改 config.json → 重启 helper →
    立即调 `validate_config`"。
