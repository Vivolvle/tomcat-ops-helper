# problem.md — 使用问题登记（MCP `report_problem` 自动写入）

其他项目的 agent 在使用 tomcat-helper 的 MCP 工具过程中遇到任何不方便、缺失能力或
报错，都应通过 `report_problem` 登记，而不是默默绕过。operator 会定期处理这里的内容。

<!-- 新条目由 server.exe 追加在文件末尾；上面的说明不要删。 -->

## [2026-09-15 20:03:23]
- 来源: user-feedback
- 工具: tomcat-helper_exec
- 问题: 本次使用体验反馈：
1. service.bat install 需要同时设置 JAVA_HOME 和 CATALINA_HOME 环境变量才能工作，文档/报错提示可更明确（当前只提示 CATALINA_HOME）
2. service.bat 不支持 start 参数，只能用 net start 启动服务，这与 Linux 下的 service.sh start 不一致，容易让人困惑
3. tomcat10w.exe 需要重命名为 <服务名>w.exe 才能绑定服务，这个约定没有明显文档提示，建议在安装服务时自动复制一份或给出明确提示
4. config.json 修改后不支持热重载，需重启 helper 进程，建议增加 reload 配置的工具或监听文件变更
5. tomcat-helper_status 显示的 tomcat_home 似乎写死为 C:\tomcat，没有读取 config.json 中的 tomcat_home，导致初始状态检查不准确
6. 缺少验证 config.json 配置是否生效的工具（如 validate_config 已有，但无法验证修改后是否生效，除非重启）

