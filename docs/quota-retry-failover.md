# 额度耗尽后的请求内切号

当请求发出后才收到额度耗尽错误，旧逻辑只更新账号额度状态，没有重新准备会话切号方案；持久化账号归属仍然指向原账号，导致后续重试无法选出新账号。

端点先按已有重试预算及输出状态判断能否重试；允许重试时，若当前会话账号已被确认额度耗尽，再复用正常切号流程。额度耗尽切号可由 `codex_session_failover_enabled` 或宽松模式 `codex_fork_account_fallback_enabled` 启用。API 中转豁免请求和没有独立会话归属的后台被动请求不走这条持久化迁移流程。

宽松模式还识别本次请求中的临时排除：例如 BPS 图片上传返回 429，账号本身仍健康，但重试排除集合已经包含该账号。以前这种情况继续固定原账号，产生 `root_owner_unavailable / request_excluded / retry=stop`。现在，对已观察到的可重试 429、5xx、传输故障或首字超时，在端点决定继续重试时走正常迁移，选择另一符合条件的账号。无需把原账号标记为额度耗尽；裸排除标记、参数错误或策略拒绝不能单独触发这条逻辑。

- HTTP 请求使用 `max_rate_limit_retries`，与 `max_retries` 独立。例如普通重试 0、429 重试 1，允许 429 后再尝试一次。
- 原生 WebSocket 入站仍使用 `codex_ws_silent_retry_enabled` / `codex_ws_silent_max_retries`；不将 HTTP 的次数强加到原生 WebSocket。现有持续重试策略的选择和预算保持有效。
- `usage_limit_reached` 及真实 HTTP/握手 429 响应头确认的额度窗口耗尽，不再被粘滞策略当成普通临时限流。流内错误读取完整结构，保留额度分类。
- 普通流内 429 的粘滞同号重试在预算允许时延后设置临时冷却，避免在重试前把自身排除；预算耗尽仍记录冷却。
- 目标账号继续满足原有分组、模型、容量及窗口授权条件，标签不参与切号匹配。没有符合条件的账号返回现有 `no_available_account`；上下文不能迁移时保留具体的上下文错误，且不发送到其他账号。
- 切号通过原有数据库事务更新归属和代次，重建出站会话/轮次身份、重置窗口序号，并清理旧 `turn-state`。新账号返回真实状态后签发新的客户端别名；旧别名不能恢复旧账号的状态。
- input 按当前“完整保留”设置处理，工具声明保留检查继续执行。已经向客户端发送回答内容后，不自动重放该请求。

回归覆盖 HTTP 429、SSE 额度失败、原生 WebSocket、真实 WebSocket 上游、生命周期帧先于失败、压缩请求、临时同号限流、零预算、重试目标再次额度耗尽、关闭切号、分组/模型不匹配、标签不同仍可切号、不可迁移上下文、API 中转豁免、首请求刚建立归属、连续迁移、重启后的归属恢复，以及切号后的工具、身份、窗口序号和 turn-state 别名。

相关测试位于 `proxy/session_quota_retry_test.go` 与 `proxy/wsrelay/session_failover_epoch_test.go`；沿用已有切号、额度分类、持续重试、turn-state 持久化与身份隔离测试。

`proxy/relaxed_retry_failover_test.go` 另行覆盖临时排除、宽松开关、连续迁移及真实附件上传路径：原账号上传 429，目标账号上传成功后只向目标账号发起一次推理。诊断中的 `account_failover.trigger_reason=request_excluded`、`enabled_by=relaxed_mode` 和目标账号 ID 用于核对这条路径。NewAPI 继续读取 Codex2API 的真实调度结果，无需屏蔽错误；没有符合条件的候选或重试预算耗尽时仍返回失败。
