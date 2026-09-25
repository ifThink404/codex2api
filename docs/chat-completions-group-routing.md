# Chat Completions 分流

API Key 的“无指纹 / Chat 请求分流组”继续使用 `limits.no_affinity_group_ids` 保存，不增加配置字段或数据表。留空时不启用分流。

- 新会话请求的实际入口路径为 `/v1/chat/completions` 时优先进入分流组，携带 Codex 指纹或 `X-Codex2API-Affinity-Key` 也不改变该规则。查询参数不影响匹配，不信任 `X-Original-URL` 等客户端声明。
- 其他入口维持指纹分流：无指纹走分流组，有指纹走原分组；原分组不限时排除分流组。
- Chat 请求在 API 中转豁免判断之前查询当前 API Key 及已解析会话范围的持久归属，随后才查运行期归属。已有绑定保留原账号的分组集合，包括重启后恢复的绑定、关联根和 fork 父系；新配置不会主动跨组迁移旧对话。原账号丢失或归属查询失败时停止选号。
- 保留绑定不会恢复被撤销的账号权限，也不绕过模型、套餐、渠道、容量或并发检查。已有自动换号仍需满足账号分组完全一致（标签不参与）、上下文清理和授权迁移等约束。
- 分流组无可用账号时不回落到普通组。真实会话身份、指纹、窗口序号及 Chat/Responses 协议转换保持现有处理。

API 中转识别、普通选号、原样 API 透传、重试和换号候选使用相同的分流结果。原样透传先完成分流再选号，仅从副本读取身份字段并记录分类，原始请求及响应字节仍保持不变。`diagnostics.group_routing.reason` 可为 `chat_completions_path`、`existing_session_binding`、`no_request_fingerprint`、`request_fingerprint`、`group_routing_owner_lookup_failed` 或 `group_routing_owner_unavailable`；已有绑定额外记录内部账号 ID，不输出会话原值、分组快照或凭据。
