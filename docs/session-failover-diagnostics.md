# 切号失败诊断

当原账号不可用、切号选择结束且 `account_failover.result=no_safe_candidate` 时，返回：

- `code=no_available_account`，消息“暂无可用账号。请稍后手动重试或联系管理员。”
- 尚未发出响应头的 HTTP 请求使用 400 和 `X-Should-Retry: false`，错误 `details.retryable=false`。这是会话无法满足切号条件的终止结果，避免通用 503 触发自动重试；恢复条件后仍可手动重试。
- 已开始的 SSE 发出对应终止错误事件；WebSocket 发出同一错误并使用 1008 关闭，而非 1013“稍后重试”。
- 只针对切号无候选；其他调度故障继续使用原来的错误与重试策略。不依赖 NewAPI 识别私有调度诊断来停止这类 503 重试（部分旧版不认识分组/标签排除原因）。第三方客户端或管理员自定义的重试规则仍可能主动重新发起请求。

管理日志新增 `account_failover.selection`：

- `required_group_ids`：原账号的分组匹配要求，`match_mode=exact_groups` 表示账号分组集合必须完全一致。标签不参与筛选或提交前复查，新日志不再生成 `required_tags`；旧日志中的该字段保留原始历史含义。
- 宽松模式使用 `match_mode=relaxed_key_scope`，不设置旧账号的 `required_group_ids`；不要求新旧账号同组，但 Key 分组权限始终参与筛选。授权范围外的候选记录为 `api_key_scope_mismatch`。
- `attempts`：本轮调用选择器的次数。
- `rejection_counts`：实际选择过程观察到的排除次数，同一个账号可被重复检查，因此不是独立账号数。
- `candidates`：最多 20 条不同“账号 ID + 排除原因”样本；附带诊断收尾时读取的候选分组与标签。标签仅供识别账号，不是切号条件。只展示实际检查到的失败条件，不推测未执行的筛选结果。
- `omitted_observations`：样本上限之外未展开的排除观察次数。`scheduler_incomplete` 表示索引调度等原因导致诊断可能不完整，不能当作所有账号的完整清单。
- 每份分组/标签最多 32 项，标签脱敏并截断；`truncated` 表示字段超出限制。

冷却、暂停、请求排除、分组不匹配会尽量关联具体账号 ID；出站身份建立失败、会话容量拒绝等后续失败也会记录。扩展详情只在切号选择器启用，不额外扫描账号池、不查询历史用量、不发起网络请求；最多按内存索引读取 20 个候选样本的标签和分组。新请求不再因为标签不同产生 `account_tags_mismatch` 或 `account_tags_changed`。

详情保存在原有诊断 JSON 中，无新增数据表。客户端只得到通用错误和请求关联 ID，不返回账号 ID、标签、分组、凭据或筛选明细。旧日志无法补齐当时未采集的候选明细。

BPS 附件上传失败后重试选号无候选时，客户端保留最后一个上传错误及 HTTP 状态。服务错误日志另记一条 `code=codex_dispatch_bps_upload_retry_unavailable`、`stage=dispatch`，携带最终的候选淘汰原因、原上传失败阶段和关联请求 ID。它是选号终止事件，不新增上游尝试或用量计费；只看第 1 次尝试的使用日志时不会再漏掉其后的选号结果。
