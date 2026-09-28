# 关闭请求明细时保留轻量计量

系统设置 → 常规 → 运行时优化：

- **使用日志写入**（`usage_log_mode`）：`full` 记录全部请求明细；`errors` 只记录错误；`off` 不写 `usage_logs`。
- **轻量计量**（`usage_metering_enabled`，默认开启）：明细被省略的请求仍写一条精简计量记录，保留计费、Token 统计和窗口额度。

`PUT /api/admin/settings` 支持单独更新 `usage_metering_enabled`。开关单独保存在 `system_settings.usage_metering_enabled`，保存成功后当前实例立即生效，其他实例重启后读取。同时修改两项时先开启计量再关闭明细、先恢复明细再关闭计量，不会出现未计量的空档。

## 组合

| 明细模式 | 轻量计量 | 实际写入 |
| --- | --- | --- |
| `full` | 开或关 | 每个事件一条完整日志，不重复写计量 |
| `errors` | 开 | 错误写完整日志，其余事件写精简计量 |
| `off` | 开 | 只写精简计量 |
| `errors` / `off` | 关 | 旧行为：API Key 累计金额照扣，但被省略事件的 Token 统计、账号/Key 窗口额度会漏算 |

## 存储与统计口径

- `usage_metering` 表只保存金额、各类 Token、时间、账号与凭据代次、API Key ID/名称/掩码、渠道、模型、端点、状态码、耗时及少量统计分类；不保存 IP、UA、诊断、错误正文或追踪 ID。
- `usage_metered_events` 视图用 `UNION ALL` 合并 `usage_logs` 与 `usage_metering`，仅用于统计：仪表盘汇总、图表、账号窗口与计费、API Key 窗口额度与统计、自助用量页。请求列表、明细、导出、错误页以及带搜索/维度筛选的用量统计仍只读 `usage_logs`。
- 每个事件只落在一张表里，切换模式不会重复计数。计量行与明细行、API Key 扣费、scope 计数和累计汇总在同一事务提交。
- 清空请求日志只把被删除的明细计入累计基线；轻量计量行保持在线，继续参与滑动窗口，不会被重复计入基线。计量行暂不自动过期。
- PostgreSQL 上 `usage_logs` 被视图引用的列不能直接 `ALTER COLUMN ... TYPE`，迁移中的宽度调整已改为仅在类型不同时执行；以后新增此类迁移也需同样处理。
