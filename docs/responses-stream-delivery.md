# Responses 生成与下游交付诊断

原生 Codex `/v1/responses` 的 HTTP/SSE 下游、HTTP 或 WebSocket 上游路径，在请求诊断的 `upstream.stream_delivery` 中记录终态与本地交付状态。其他协议转换路径未新增此段诊断，缺失不代表成功。

- `terminal_event`、`response_status`、`incomplete_reason`：最终进入转发回调的终态及生成状态。`incomplete` 不等于完整生成成功。
- `terminal_received_at_unix_ms`：转发回调观察到终态的时刻。
- `usage_source`：终态中是否存在上游 usage 对象，取值 `upstream` 或 `missing`，不是本地账单估算来源。
- `usage_received_after_cancel`：终态用量是否在已观察到下游请求取消后取得。
- `cancel_observed_at_unix_ms`：观察到取消的时刻，不保证是取消实际发生时间。
- `terminal_write`：`not_attempted`、`not_seen`、`accepted`、`buffered`、`failed`。
- `terminal_write_at_unix_ms`：本地写入调用／缓冲提交结果的记录时刻。
- `downstream_status`：`write_accepted`、`client_canceled`、`write_failed`、`buffered`、`terminal_not_written`。
- `write_error`：经过脱敏并限制长度的本地写回错误。

`accepted` 仅代表本地写入调用成功，不代表对端应用收到或消费了完成帧。连续重试的私有缓冲不能算作交付，只有提交到下游后才更新该状态。

现有“上游生成终态优先”的用量日志状态码和断线后最多五秒提取用量机制不变；请结合新增交付状态判断。该诊断不改变重试、连接池、账号粘性和收费策略，也不记录模型正文。

使用 NewAPI 的请求 ID 与 Codex2API `diagnostics.newapi_request_id` 对照。历史请求没有这些字段，无法据此恢复其最终帧或精确断线时序。

## 可选的前置事件立即透传

系统设置的 Codex「压缩与兼容开关」新增「前置事件立即透传（含 response.created）」。API 字段为 `codex_early_sse_passthrough_enabled`，默认 `false`，保存在 PostgreSQL / SQLite 系统设置中，重启保留；保存成功后，后续流转发尝试使用新值，已经开始转发的流不会中途切换。

开启后，Responses HTTP/SSE 下游可提前收到上游的 `response.created`、`response.in_progress` 和前置元数据。覆盖原生 Codex（包括 BPS 共享转发路径）及 Responses API 中转；HTTP / WebSocket 上游均由同一流转发逻辑处理。只转发上游实际发送的事件，不伪造 `response.created`。非流式 JSON 和 Chat / Claude 等协议转换不新增 Responses 事件。输出过滤仍按原策略执行。

- 提前发出响应体会提交 HTTP 200。此后错误通过流内错误或断流表现，无法改回 HTTP 错误，也不能再透明切号或进行超窗压缩重试。开关关闭时保留原有前置缓冲。
- 「持续重试」优先：开启时必须保留整次尝试的私有缓冲，因此本开关即使保存为开启，也不会提前发送前置事件。界面会显示提示；要立即透传需关闭持续重试。
- 独立于「向 NewAPI 上报宽松首响应」（旧字段 `codex_preflight_sse_passthrough_enabled`）与首字统计口径。旧开关仍只上报计时；新开关不会把 `response.created` 算作模型 token，也不改变 token 提取或计费计算。
- 立即透传可能让 NewAPI 按首个 SSE 事件统计的首包时间缩短，但不让模型更早生成正文。提前提交响应头后，后续元数据中的计时或 turn-state 响应头不能再补发。
