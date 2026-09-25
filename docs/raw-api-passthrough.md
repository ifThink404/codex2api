# API 账号原样透传

在「账号 → OpenAI Responses API／中转账号 → 新增或编辑」启用 **原样 API 透传（HTTP / SSE）**。默认关闭，现有账号不会自动开启；配置保存在账号凭据的 `raw_passthrough_enabled` 布尔字段中。

例如账号 13：部署此版本后，编辑账号 13，确认上游地址、密钥、代理和模型列表，开启开关并保存。没有开启这个开关的 API 账号仍使用原来的兼容处理流程。

## 路由规则

- API Key 的账号、分组、渠道与模型权限仍生效。
- 先读取原始会话字段及经过验证的 NewAPI 元数据，执行 API Key 的分组分流，再在该组内优先选择模型列表匹配的透传账号。其他组的透传账号不会接走当前请求；不使用模型映射来改名匹配。
- Responses 等入口按现有指纹规则选组；新的 `/v1/chat/completions` 请求走分流组，已有 Chat 会话保留原绑定账号的分组。组内没有匹配的透传账号时继续该组的普通流程；归属查询失败或原绑定账号丢失时不跨组兜底。
- 原始模型名称没有直接命中时，普通转换流程会排除已开启透传的账号，避免模型别名映射后重新选中该账号并改写请求。其他未开启透传的账号仍可按原有映射提供服务。若上游本身支持某个别名，应把它直接加入该透传账号的模型列表；发送时仍保留这个别名。
- 如果匹配的透传账号都被禁用、不可用或并发已满，请求直接失败，不退回兼容处理。透传请求不会自动换号重试。
- 多个透传账号匹配时，使用原账号调度器的可用性、权重和并发规则；本模式不沿用 Codex 会话亲和绑定。需要固定到一个上游时，请通过 API Key 的账号／分组权限只允许该上游。

## 保留与跳过的内容

支持 POST `/v1/responses`、`/v1/responses/compact`、`/v1/chat/completions`、`/v1/messages`，以及它们已有的无 `/v1` 前缀路由。保持请求实际路径及查询参数；上游地址已经以 `/v1` 结尾时不会再重复拼接 `/v1`。

请求正文按原始字节发送，包括模型名、身份字段、提示词、工具、图片及扩展字段。不补齐或重写 ID，不上传 BPS 附件，不转换协议，不做模型映射。从正文副本读取模型、会话及轮次元数据，用于分流、权限检查和分类日志，仍要求有效 JSON 和唯一的字符串 `model` 字段。gzip 请求会解压副本用于调度，发送的仍是原始压缩字节。

响应保留上游 HTTP 状态、端到端响应头、尾部头和正文，包括上游错误与 SSE 事件。不会重建 JSON、隐藏工具或注入结束事件。客户端断开时取消上游请求。

这是 HTTP 代理，不是 TCP 字节隧道：目标地址和 Host 使用账号配置；入站网关密钥替换为账号的 Bearer 密钥；账号自定义请求头按配置覆盖。入站 Cookie、网关签名头、转发来源头及 HTTP 逐跳头不会带到上游。HTTP 分块边界和请求头大小写可能由 HTTP 库调整。

WebSocket、模型目录以及图像／视频生成等其他接口不属于此开关的范围，继续使用原流程。

## 用量与诊断

API Key 鉴权、模型请求次数和并发／预算限制继续生效。网关只观察响应副本记录上游用量，不修改下游收到的用量；优先计费仍依据用户请求的 `service_tier`，不会因为上游默认 fast 自动变成双倍计费。

支持观察 Responses、Chat Completions、Messages 的常见用量结构与 SSE 事件。为控制内存，单个 JSON 正文、SSE 数据行或完整事件最多观察 4 MiB；gzip 响应的压缩及解压副本也各限 4 MiB。无法取得用量时记录未观察到，不编造 token 数，也不改变正文。流式请求只在上游返回 usage 时记录用量，不主动追加 `include_usage`。

透传仍记录 `incoming`、`resolved`、`group_routing` 和请求类型。用户主线程、关联后台、独立后台等沿用普通请求的身份分类规则；只有 UA、缺少可靠会话证据时仍为 `unknown`，不会强行标成用户请求。该诊断过程不补齐、改写出站身份，也不启用 Codex 状态映射或窗口租约。

请求诊断中的 `raw_passthrough`，包含开关、请求／响应字节数、用量来源和传输中断原因；`selected_account_id` 显示实际选中的账号。启用账号 13 后，可据此确认实际走了账号 13，而非旧 BPS 绑定账号。

流式 HTTP 200 只代表开始响应时的状态，不代表生成一定成功。`raw_passthrough` 另外记录：

- `terminal_event`、`response_status`、`incomplete_reason`：完整 SSE 结束事件和生成状态；失败消息记录在经过脱敏、限长的 `terminal_error`。
- `stream_outcome`：`completed`、`failed`、`incomplete`、`interrupted` 或 `unknown`。只有完整的协议结束事件可证明完成；收到 usage 或未以空行结束的事件都不能证明完成。
- `stream_end`：本次转发因 `eof`、`read_error` 或 `write_error` 结束；`read_error`／`write_error` 保存底层异常的类别、类型和经过脱敏、限长的消息。
- `client_canceled`、`request_context_error`：入站请求是否被取消，以及取消／截止时间错误。入站可能来自 NewAPI 等中转层，不一定直接来自终端用户。

完整成功事件已被下游写入接口接受后出现的读取异常，不再计为 `upstream_read_failed`，但仍保留上述传输诊断。这不证明终端用户已阅读或收到全部内容；下游写入明确失败仍记录为错误。未完成时取消记录为 `downstream_canceled`，请求超时记录为 `request_deadline_exceeded`，真实的读取中断仍为 `upstream_read_failed`。上游 `response.failed`／`response.incomplete` 即使使用 HTTP 200 也分别记录为 `upstream_response_failed`／`upstream_response_incomplete`，不会被后续 `[DONE]` 覆盖。

诊断不会提前截断流、追加错误事件、自动重试或改变原 HTTP 状态；新增字段仅对部署后的请求生效，无法追溯判断旧日志中缺失的结束信息。
