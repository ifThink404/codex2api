# 代理出口 Web Search 地区

系统设置 → Codex「按代理出口地区设置 Web Search 位置」（`codex_web_search_proxy_location`），默认关闭。

- 数据来源：代理单测 / 批量测试成功时写入 `proxies` 行，与既有 `test_timezone` 同一处保存，
  不新增独立地区表：`test_country_code`（ISO 两位代码，与界面语言无关）、`test_region`、`test_city`。
  展示用的 `test_location` 仍按管理页语言；省州/城市只在英文检测（`lang` 为空或 `en`）时保存，
  避免把本地化地名写入 `user_location`。测试失败或修改代理 URL 清空这些字段。
- 运行时：代理池重载时把地区快照载入 `auth.Store`，出站只查内存，不查库、不联网。
- 改写：只替换已有顶层 `web_search` / `web_search_*` 工具的 `user_location`（`type: approximate`，
  缺失字段省略），不新增工具、不改 `input`。时区优先账号绑定时区（`EffectiveCodexTimezone`），
  其次代理出口时区，与 `environment_context` 改写保持一致。
- 不改写：开关关闭、直连、不在代理表中的代理、Resin 出口（出口未知）、代理没有国家/省州/城市。
- 适用于 Codex Responses 的 HTTP 与 WebSocket 上游（两者共用 `ExecuteRequest` 分叉前改写）以及 compact 请求。
