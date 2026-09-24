# sever 工具转换与选号诊断

## 工具名称与 namespace

Chat Completions 的工具名称检查覆盖标准 `function.name`、顶层 `name`、省略或使用 null 的 `type`，以及 namespace 内的声明。Responses 同时检查 `tools`、`input[].additional_tools` 和 `tool_search_output` 内的声明。名称缺失时返回包含原请求字段路径的 400，不编造工具名，也不丢弃该工具后继续请求。

Chat 普通函数调用的 `namespace` 会保留在 Responses 历史项及流式 Chat 工具首块中；非流式响应继续保留此字段。已有 namespace 原样传递，缺失 namespace 不根据工具名猜测。

## 查看新请求的工具诊断

- `diagnostics.tool_protocol`：网关入站工具定义和历史调用的形态；Chat 和 Responses 均支持。
- `diagnostics.upstream.tool_protocol`：本次上游尝试最终发出请求的形态，HTTP / WS 共用采集器。是否实际发出仍以 `send_phase` 为准。
- 服务错误详情的 `tool_protocol`：本地校验失败时也保留入站诊断。

每条样本包含字段路径、工具类别、名称与 namespace 的 `absent` / `empty` / `invalid_type` / `present` 状态及非空值的 SHA-256 截断哈希。标准 Chat 名称位于 `nested_name_*`，Responses 名称位于 `name_*`；哈希相同说明转换前后的名称一致。

`missing_namespace` 仅表示：历史调用未携带 namespace，而当前声明中同名工具仅存在于 namespace 内。它用于定位，不能证明调用来源或代替恢复原始 namespace。存在同名默认函数时不做此标记。

最多保留 8 个声明样本、8 个调用样本和 8 个异常样本；`omitted_issues` 记录省略的异常数量。工具扫描上限为 4096 项、namespace 深度为 8，达到限制时设置 `scan_truncated`。不记录工具描述、参数 schema、调用参数、执行结果或对话内容。

## 503 选号诊断

服务错误与使用日志增加 `dispatch_selection`：

- `pinned_account_id` / `root_account_id`：固定账号及本次选号观察到的绑定账号。
- `candidates.rejection_counts`：拒绝原因的观察次数，同一账号可能被检查多次。
- `candidates.samples`：最多 20 个账号 ID 与拒绝原因样本，超出时记录 `omitted_observations`。
- `state`：相关账号的 5h / 7d 阻塞标志、自动暂停标志、冷却截止时间及白名单化的冷却原因。
- `compaction`：已知压缩状态的首选账号和兼容域。BPS / native 域直接标记，其他域用哈希表示。

懒加载路径保留原有 `lazy_account_unavailable` 标签，同时记录 `account_disabled`、`account_paused`、`account_error`、`account_banned`、`account_cooldown`、`account_usage_exhausted` 或 `credential_unavailable` 等具体原因。重试会清除上一尝试的样本，继续采集当前尝试；这些记录不会改变选号、限额、绑定或压缩兼容规则。

以上信息供管理员排查，公开错误正文不返回候选账号明细。不需要数据库表迁移；仅部署后的新请求有新增诊断，旧日志无法回填。
