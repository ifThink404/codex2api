# Chat Completions 分流

API Key 的“账号分流组”继续使用 `limits.no_affinity_group_ids` 保存，不增加配置字段或数据表。留空时不启用分流。

以下主组／分流组路由和旧分组保留规则仅在宽松模式关闭时执行。宽松模式开启时，Chat、Responses、Compact、Messages 及原样透传可在 Key 授权范围内跨组选号，不为保留旧分组而查询或锁定旧账号。Key 分组权限始终生效，实际会话归属与迁移校验仍由会话流程负责。诊断标记 `group_routing.reason=relaxed_key_scope`，非分组的账号资格限制保持生效。

先检查用户原始请求：会话 ID 或思考等级任一缺失时，只使用分流组，即使带有 Codex 指纹或本地 affinity 标识。思考等级读取原始 `reasoning.effort` / `reasoning_effort`，未传、`null`、空字符串和纯空白均视为缺失；显式 `low` 算已提供。模型映射、默认补 `low`、历史消息中的配置和后续生成的身份都不能覆盖这个判断。会话识别支持原始会话/线程请求头及 `client_metadata`、turn metadata；单独的 `prompt_cache_key`、请求 ID、BPS `task_id` 不算会话 ID。

HTTP 入站记录一次，重试复用原始判断；WebSocket 每个 `response.create` 重新记录，不沿用上一帧的思考等级或会话元数据。两项均已提供时，继续使用以下规则：

- 新会话请求的实际入口路径为 `/v1/chat/completions` 时优先进入分流组，携带 Codex 指纹或 `X-Codex2API-Affinity-Key` 也不改变该规则。查询参数不影响匹配，不信任 `X-Original-URL` 等客户端声明。
- 其他入口维持指纹分流：无指纹走分流组，有指纹走原分组；原分组不限时排除分流组。
- Chat 请求在 API 中转豁免判断之前查询当前 API Key 及已解析会话范围的持久归属，随后才查运行期归属。字段齐全的已有绑定保留原账号的分组集合，包括重启后恢复的绑定、关联根和 fork 父系。原账号丢失或归属查询失败时停止选号。缺字段时收窄到分流组，但不绕过原有会话归属约束；受保护的旧会话不能安全迁移时仍拒绝请求。
- 保留绑定不会恢复被撤销的账号权限，也不绕过模型、套餐、渠道、容量或并发检查。已有自动换号仍需满足账号分组完全一致（标签不参与）、上下文清理和授权迁移等约束。
- 分流组无可用账号时不回落到普通组。真实会话身份、指纹、窗口序号及 Chat/Responses 协议转换保持现有处理。

API 中转识别、普通选号、原样 API 透传、重试和换号候选使用相同的分流结果。原样透传先完成分流再选号，仅从副本（gzip 时为解压副本）读取身份字段并记录分类，原始请求及响应字节仍保持不变。`diagnostics.group_routing.reason` 新增 `missing_session_id`、`missing_reasoning_effort`、`missing_session_and_reasoning_effort`，并保留 `chat_completions_path`、`existing_session_binding`、`no_request_fingerprint`、`request_fingerprint`、`group_routing_owner_lookup_failed` 和 `group_routing_owner_unavailable`；已有绑定额外记录内部账号 ID，不输出会话原值、分组快照或凭据。
