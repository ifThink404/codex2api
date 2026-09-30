# 服务错误日志

管理端「系统运维 → 服务错误」（`/ops/service-errors`）记录 **Codex2API 自身产生的请求拒绝或服务异常**，与既有的上游错误日志分开存储。

## 采集边界

- 覆盖 `/v1`、`/v1beta`、`/backend-api/codex` 及对应无前缀代理路由。全局 RPM 限流、请求体大小校验等在 API Key 鉴权前的拒绝也会记录。
- 通过 `api.SendError` / `api.SendErrorWithStatus` 发出的本地错误即时采集；未经这两个函数、直接写出的旧式错误响应由入口中间件兜底（最多暂存 8 KiB 错误正文用于解析）。
- Responses / Realtime WebSocket 的本地错误帧按逻辑请求（每帧）采集，不等连接关闭；同一请求只记一次。
- 已向上游发出请求后，只有本地调度失败（如重试耗尽后无可用账号）仍记为服务错误；上游账号的 401、429、额度耗尽、过载等留在原「错误日志」（用量日志）。
- 不采集管理页面请求、成功请求正文、Authorization、密钥或完整请求头。NewAPI 请求 ID 仅在签名元数据已验证时才标记为已验证，未验证的请求头只用于搜索。
- 不改变客户端的状态码、错误正文、重试策略或账号健康判断；不计入用量、费用或账号错误统计。

## 字段

时间、状态码、错误码/类型/消息（脱敏，最多 2 KiB）、阶段（`authentication` / `rate_limit` / `policy` / `dispatch` / `validation` / `internal`）、方法与端点、传输方式（HTTP/SSE/WS）、模型、耗时、API Key ID/名称、请求 ID（与响应头 `X-Codex2API-Request-ID` 一致）、NewAPI 请求 ID 与已验证用户，以及客户端自报的 UA / Originator / Version 与 turn metadata 中的线程来源。

## 性能与保留

- 单进程有界队列 512 条、单写入协程、每批最多 64 条，不足一批每秒写入；请求热路径不等待数据库。队列满时丢弃并计数。
- SQLite 与 PostgreSQL 共用 `service_error_events` 表，保留 7 天且最多约 100,000 条，每分钟清理。
- 页面展示本次进程启动后的待写入、丢弃、写入失败计数；正常退出最多等待 3 秒排空队列。

## 重复错误收敛

默认「收敛显示」：按调用方（已验证 NewAPI 用户，否则线程 ID）、API Key、端点、传输方式、模型、状态、阶段、错误码与原因分组，请求 ID、时间、耗时与名称不拆组，错误说明中的当前请求 ID 会归一化。次数覆盖整个筛选时间段；点击次数以 `grouped=false&group_key=<key>` 查看该组原始请求。

## 管理接口

`GET /api/admin/ops/service-errors`：成对 RFC3339 `start/end`（默认最近一小时，最多 7 天）、`status`（`429`/`4xx`/`5xx`）、`stage`、`request_id`（精确匹配 Codex2API 或 NewAPI 请求 ID）、`grouped`、`group_key`、`cursor`、`limit`（默认 20，上限 100）。返回 `items`、`next_cursor`、`summary`、`collector`。分组与逐条视图的游标不能混用。
