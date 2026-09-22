# Codex 容量不足重试

设置 → 过载自动暂停 → **容量不足自动重试**（`codex_capacity_retry_enabled`）。新安装和旧数据库升级后均默认关闭，与过载熔断独立。

| 开关 | 网关行为 | 最终下发 |
| --- | --- | --- |
| 关闭 | 网关不补试 | 按下述规则交给客户端处理 |
| 开启 | 仅按对应通道现有的有限次数补试 | 耗尽后按下述规则交给客户端处理 |

- HTTP 请求使用“最大重试次数”，HTTP 429 使用独立的“429 重试次数”；原生 Responses WebSocket 使用已有的 WS 静默重试开关和次数。次数 1 表示首次请求之后最多补试一次，不保证账号/连接不可用时一定能补试。
- 容量错误不会因“持续重试”、超级模式或精确错误码选择器变成无限重试。其他错误的重试规则不变。
- 已经发给客户端的流式内容不重放；未提交的暂存内容可以在有限预算内丢弃重试。
- 临时 `server_is_overloaded`（如 `Our servers are currently overloaded`）及 `slow_down` 在客户端副本投影为 `rate_limit_exceeded` / `rate_limit_error`，固定文案说明服务器临时过载，并包含 `Please try again in 30s.`。30 秒是网关缺省建议，不是推断出的上游恢复时间。若提供可解析的重试秒数/毫秒数，则保留；HTTP 返回头的有效 `Retry-After` 优先。重试建议接受范围不超过 5 分钟。
- 兼容码有明确原因：本机官方 0.154 源码将 `server_is_overloaded` 和 `slow_down` 都归为不可重试；新版源码只将前者归为不可重试。`rate_limit_exceeded` 的消息重试时间在两个版本均可解析。只改回 500 或添加响应头不能恢复流内错误的等待重试。
- Responses 流请求返回 `response.failed` 事件，保留失败状态而不伪装成功；尚未提交头时建立 HTTP 200 SSE，普通 JSON 请求保留上游 429/5xx，未知状态回退 503，并携带 `Retry-After`。WS 临时过载也发送 `response.failed`，需要关闭时使用 1013。客户端自己的重试次数、取消和版本仍决定最终是否继续。
- 明确的 `Selected model is at capacity. Please try a different model.` 或模型已关闭提示仍为终止错误：HTTP 400、无 `Retry-After`，WS 1008。不会将这类模型容量错误改为持续重试。
- 本地上游用量、故障、会话过载统计保留原始 500 / `server_is_overloaded` 与原消息；客户端兼容转换不回写原始事件，不影响网关重试预算或账号惩罚规则。原始正文、任意 details、组织与追踪信息不回传。WS 通用错误隐藏不会覆盖重试提示。
- 识别结构化错误中的 `server_is_overloaded` / `slow_down`，以及明确的 `Selected model is at capacity` 文案；不把普通 500、余额不足或所有 429 当作模型容量不足。

该设置仅控制网关内部补试，与客户端自动重试分开。已经对外发送的部分输出不会由网关隐藏重放，是否重试由客户端决定。
