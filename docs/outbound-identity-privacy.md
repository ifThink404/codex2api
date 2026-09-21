# 出站身份一致性与隐私

本策略只处理上游协议身份与元数据。内部找根、账号绑定、计费、窗口归属继续读取原始入站身份；改写发生在完成路由后的出站副本。用户侧响应别名与官方侧身份映射保持独立。

已登记的项目/工作区 UUID 使用[账号级双向映射](codex-project-identity.md)：已识别协议元数据保留别名，业务正文使用同一别名，回传业务内容恢复客户端原值；它是业务内容保留规则的明确例外。项目映射不会把所有 UUID 或任意业务字段都当作身份，未知控制容器和不支持的顶层字段仍不恢复。

## 共同出口

`PrepareCodexOutboundMetadata` 解析指定身份载体、统一别名并过滤未登记的控制元数据。规范正文快照优先于平铺兼容字段；当前帧有快照时不回填旧握手字段。官方定义的嵌套结构按各自 schema 保留；未知身份对象、数组和 JSON 字符串容器仍移除。正文 input、工具参数、工具结果、工具 schema 不参与本隐私过滤器的身份遍历（其他既有协议转换规则仍可能处理工具格式）。

支持的出站顶层字段集中在 `codexOutboundRequestFields`；支持的元数据字段集中在 `codexOutboundMetadataFields`。增加合法协议参数时应同时登记、验证类型并补充最终发送测试。Responses Lite 能力标记单独保留，不能当作身份删除。

标准会话/线程/窗口/轮次按持久化映射改写，`parent_turn_id` 读取父轮次已有映射。独立 `client_request_id` 有单独稳定映射；等于线程标识时沿用线程映射。重试不会重新给别名取别名。

`FinalizeCodexOutboundMetadata` 从映射后的快照生成头部和平铺字段，覆盖账号自定义头造成的身份冲突。`ValidateCodexOutboundMetadata` 在发送前检查所有同时存在的标准载体；不一致则停止发送，错误只含字段名。入站的身份重复键在路由前检查，冲突拒绝；业务 input 内部的键不受此检查影响。

最后发送的 `client_metadata` 始终是字符串字典；兼容对象输入会转换成 JSON 字符串。完整快照内部的布尔、整数、对象保留原类型。`model` / `reasoning_effort` 元数据若存在，使用最终请求中的实际值；无实际 effort 时移除旧值。

## 恢复的功能字段

对照 Codex `rust-v0.155.0-alpha.9`（`434535bddfaf405a032f57be3c1096dd25ff6312`）的 [请求结构](https://github.com/openai/codex/blob/434535bddfaf405a032f57be3c1096dd25ff6312/codex-rs/codex-api/src/common.rs)、[轮次元数据](https://github.com/openai/codex/blob/434535bddfaf405a032f57be3c1096dd25ff6312/codex-rs/core/src/responses_metadata.rs)，以及公开 [Responses](https://developers.openai.com/api/reference/cli/resources/responses/methods/create)、[WebSocket](https://developers.openai.com/api/docs/guides/websocket-mode)、[compact](https://developers.openai.com/api/reference/python/resources/responses/methods/compact) 文档。moderation.policy.input/output.mode 等嵌套结构另外核对了官方 Python SDK 的 `response_create_params.py`。

| 字段 | 处理 |
|---|---|
| `access_programs.cyber` | create HTTP / WS 保留显式选择；不替用户提升等级，未知嵌套身份键不保留 |
| `generate` / `stream_id` | WS 帧保留预热语义；流 ID 按账号、调用方、线程稳定映射，事件（含错误、嵌套元数据）还原客户端 ID；其他流的事件终止连接并禁止自动重放 |
| HTTP 上的 WS 控制 | `generate=false` 或非空 `stream_id` 返回 `websocket_required`，不能静默变成生成；普通 `generate=true` 移除 WS 信封后正常走 HTTP |
| `prompt_cache_options` | 保留 `mode`、`ttl`、`prewarm`、`comparison_response_id`；原生账号比较引用需校验用户、根、账号及切号代数后恢复，回传再映射；中转保留其提供方响应 ID 语义 |
| 公开 Responses 的 `metadata` / `moderation` | 保留业务字符串标签和官方 input/output 审核策略；metadata 中已知身份与规范快照对齐，凭据及未知 JSON 身份容器不透传 |
| 公开 Responses 的 `safety_identifier` / 旧 `user` | 使用稳定账号/调用方隔离别名，同一原值跨两字段映射相同；不涉及 `input[].role="user"`。原生 ChatGPT create 继续不发送这四个公开 API 专用字段 |
| `tool_namespaces_info` | 保留 namespace/functions、name/direct/code_mode_name/deferred/source(kind/server_name)；名称是工具引用，不改写。完整清单只进正文，不进兼容头 |
| `agent_name` | 保留 `/root` 和父子路径关系，子节点稳定映射；响应协议中的该字段隐藏，防止上游别名直接回传 |
| 分叉、触发、执行状态 | 保留 `forked_from_ordinal_exclusive`、`turn_trigger`、`sandbox_mode`、`auto_review_enabled`、`node_repl_auto_review_required`、`node_repl_disabled`、`history_ingest_requested` |
| 工作区与 extra | 恢复 `workspaces.*.has_changes`、`workspace_kind`；路径/提交仍改写、远程 URL 仍移除；不凭空添加只在 MCP 路径明确生成的 `codex_version` / `user_input_requested_during_turn` |

`PrepareCodexFunctionalFields` 在身份映射后执行一次，重试复用结果；`FinalizeCodexOutboundMetadata` 只投影，不再散列。`stream_id` 还原不递归改写模型文本或工具参数。当前 WS 连接池仍按读租约串行消费一条连接的响应，此修改不引入同一上游连接的并行多流调度。

## 传输差异

| 路径 | 行为 |
|---|---|
| HTTP Responses | 头、正文和平铺字段使用同一份映射；压缩前正文定稿，内部重发复用字节 |
| WS 握手 | 只保留连接期稳定身份；不携带 turn/root_turn/parent_turn、window/context_window、轮次时间或独立请求 ID 的旧快照 |
| WS 帧 | 每帧携带当前完整元数据；连续响应仍绑定既有连接，握手不会与新帧发生轮次冲突 |
| compact | 仅保留独立 compact 参数：model/input/instructions/previous_response_id/prompt_cache_key/prompt_cache_options/prompt_cache_retention/service_tier；不携带 create-only tools/reasoning/text/stream_options/client_metadata，已处理身份留在头部。原生已有 previous_response_id / prompt_cache_retention 兼容删除规则继续生效 |
| OpenAI Responses 账号 | 共用元数据策略，使用独立的账号/调用方映射域；保留按需注入兼容逻辑，不因头部存在就无条件添加正文 metadata |

语义参数（模型、工具、能力标记、request_kind、thread_source）继续保留。轮次时间及窗口序号沿用已有规则，以保持排序和窗口/续写兼容；不会为了保证数值不同而随机改写。官方响应 ID、文件引用和密文上下文继续按已有绑定校验与恢复逻辑处理。

## 可信来源

| 字段 | 允许来源 |
|---|---|
| Authorization / Chatgpt-Account-Id | 所选上游账号及其受信配置 |
| installation_id | 所选账号的安装标识，缺失时使用账号派生值；用户值不作为出站设备身份 |
| User-Agent / Version / Originator | 服务端配置与账号配置；版本和来源与出站 UA 配套，不再学习当前用户的客户端指纹 |
| X-Oai-Attestation | Codex Responses 只取账号自定义头中的真实凭据；未配置时省略，不使用用户值、不伪造签名 |
| Live Attestation | 选择账号后取该账号配置，或使用服务端已有生成器；服务端无可用凭据时返回不可用，不回退用户值 |
| Turn-State | 用户侧别名经过所有者、账号、代数校验后恢复的官方值；不可信值清除 |
| OpenAI-Organization / OpenAI-Project | 上游账号配置；移除当前用户透传的值 |
| Idempotency-Key | 按账号和调用方稳定派生，保持同一请求的重试幂等性 |

Live 持久化记录增加 attestation 来源标记。历史记录没有可信来源时不重放旧凭据，需要新建通话。纯 Codex Responses 不要求为缺少 Attestation 的账号额外生成令牌。

模型清单入口不再使用调用方的 `client_version` 填写上游画像；上游查询参数、Version 头和 UA 的版本保持一致。服务端内部的显式版本选择仍可用，账号配置优先。

## 兼容与迁移

持久化存储可用、账号身份完整且未显式指定旧出站模式时，新原生会话采用账号映射。旧的持久化 preserve 策略和显式 legacy/preserve 配置继续兼容，诊断中的 `account_mapping.status/version` 表示实际策略；不能将这些旧会话称为“所有标准身份都已替换”。需要完整替换旧身份时应通过已验证的切段/新会话流程，避免断开已有续写关系。

OpenAI Responses 账号的映射密钥在有存储时持久化；无存储的独立执行路径使用账号凭据派生，轮换凭据会改变这类临时映射。新旧版本之间不应交替处理同一个持续会话。

请求业务内容中的路径、源代码、用户文本继续送往模型。这是正常推理输入，不代表客户端元数据可以任意透传。

## 入口 URL 副本

在 `client_metadata`、`metadata`（包括其 JSON 字符串嵌套）及请求头中，显式的 `base_url`、`openai_base_url`、`gateway_url` 及其大小写、下划线、连字符、`X-` 头部变体用于登记入口地址。普通 `note` 或业务消息中的网址不会自行登记。

已登记地址在协议元数据及非凭据自定义头中的副本统一替换为官方基础地址：原生账号使用 `https://chatgpt.com/backend-api/codex`，API 中转使用 `https://api.openai.com/v1`。派生 `/responses`、`/responses/compact` 保持接口后缀，缺失字段不新增；实际网络目的地、Host、认证/签名/Attestation、消息及工具内容保持原处理方式。改写后的 URL 不再经过通用 `meta_` 二次映射。

反向记录保存在本次请求的上下文中，内部分支/重试复用快照；不按官方 URL 建立全局反向表，不新增持久化地址表。响应仅恢复元数据和对应头部中的已登记值，正常输出文本/工具内容不恢复。多个不同原值对应同一官方值时不猜测回填；原有持久化 `meta_` 映射仍兼容。重启后的新请求从其显式字段重新登记，不从消息历史猜测入口地址。

本地 `outbound_identity.url_mapping` 记录有界的映射、来源及改写/恢复处理次数，最多 8 个映射、每项 3 个来源；原 URL 的用户信息、查询参数和片段不进入该诊断。它不进入上游 payload。审计中的实际连接 URL 继续如实记录。

`codex_url_privacy_test.go` 覆盖请求隔离、旧映射兼容、重复键、边界及 SSE 恢复；`wsrelay/url_privacy_test.go` 在模拟上游实际捕获 HTTP/WS/compact/API 中转请求及回传，覆盖主请求和被动来源。正文 `input` 保持不变，无须放宽完整输入校验。

## 验证

API 中转的账号级身份改写同样写入本地 `outbound_identity.account_mapping`，合并相同原值/出站值的字段路径，最多保存 32 组；`prepared_headers` 表示改写准备阶段的头，是否实际发送以 `http.headers` 为准。该诊断不进入上游请求。API 专用身份头为可选载体：只在正文携带会话时记为 `body_only`，两处都没有会话时记为 `not_applicable`；正文平铺与嵌套不一致仍记为 `mismatched`，不把正常省略可选头报告成 `missing_header`。

`codex_outbound_privacy_test.go` 验证别名规范化、未知嵌套移除、幂等性、父轮次引用、独立请求 ID、账号凭据来源、重复键拒绝和发送前冲突检查。

`wsrelay/outbound_privacy_test.go` 在本地模拟上游处抓真实 HTTP/WS/compact 请求，检查不同载体一致性、账号来源的 Attestation、连接复用、动态轮次变化，以及业务输入保留。现有的 Lite、压缩、续写、切号、找根、保留 input、响应隐私测试继续运行。

`codex_outbound_schema_test.go` 进一步验证官方字符串字典、嵌套工具结构/工作区状态、模型与 effort 一致、公开 Responses 最终请求、缓存比较引用归属和回传。`wsrelay/stream_identity_test.go` 覆盖流 ID 的嵌套回显、错误事件及错误流阻断。

这些测试不调用真实官方模型；官方账号的实际凭据有效性、设备认证要求及外部反向代理追加字段仍取决于部署环境。
