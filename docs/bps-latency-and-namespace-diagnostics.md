# BPS 大图片请求与工具命名空间诊断

2026-09-25 排查记录。附件中的慢请求与工具 400、会话归属 400 是不同请求，不应合并解释。

## 已观察到的慢请求

`202609241642033825732938268d9d6CuPPK9oY`：BPS Excel，账号 10，Astra high；入站 75,774,353 bytes、374 项，68 张图片，67 次新上传、1 次复用。网关开始至完成为约 81.4 秒，终态成功。附件未提供实际首字字段，不能把总用时当作首字时间。

输入 token 的缓存命中与附件句柄缓存是两种缓存。附件缓存按账号隔离、单进程最多 256 个条目、有效期 30 分钟；重启、换号、过期、容量淘汰均可能导致重新上传。现有日志不能证明这一次的具体失效原因。

图片上传为每请求最多 100 个并发，没有账号级上传限制。图片引用现改为每个受影响输入项批量改写一次，再替换一次整体 input；避免每张图片两次扫描并复制整份请求。保留图片顺序、detail、工具 call_id、namespace、参数与未知字段，不删除历史图片。

## 计时修正

计时收集器只有 MarshalJSON、缺少 UnmarshalJSON，导致 transport 诊断经过使用日志的反序列化/序列化后所有计数变零。现已补齐，并通过真实日志组合路径测试。旧日志中的零不能用于证明没有上传、没有推理等待。

- `pre_inference_ms`：进入 BPS 执行至首次发送推理请求的准备时间。
- `image_prepare_ms` / `file_prepare_ms` / `tool_bridge_ms`：各准备阶段墙钟时间。
- `upload_ms`：所有并发上传耗时之和，不能直接与墙钟耗时相加。
- `last_inference_headers_ms` / `last_inference_first_event_ms` / `last_inference_first_content_ms`：从最近一次推理请求发送起算。
- `cache_hits` / `cache_misses` / `cache_waits`：复用、未命中及等待其他并发上传。
- `cache_expired_entries` / `cache_evictions` / `cache_capacity_bypasses`：本请求查缓存时清理的过期条目、触发的容量淘汰、全部条目仍在上传而跳过存储的次数。清理计数不等于本请求图片失效数量。
- `cache_entries_at_miss_max` / `cache_entry_limit` / `upload_concurrency_limit`：缓存压力及实际配置。

## 缺失 namespace

`202609241644510797164878268d9d6w0iZEuns` 的入站和出站均有 2 个 function_call 缺 namespace；因此缺失发生在到达 codex2api 之前。另一份 Word 400 入站/出站均为 83 个缺失；仅凭这份诊断仍不能确定该 400 的错误正文。

BPS 请求转换会从顶层 tools、additional_tools 和 tool_search_output 中收集声明，仅在相同调用类型及工具名唯一对应一个明确 namespace 时补齐缺失字段。同名默认工具、多 namespace 重名、未知工具、过深/截断声明均不推测；已有字段不覆盖，工具参数与结果不改。`bps_compat.tool_namespace_repair` 保存修复及未解决数量和有限哈希样本。

## 会话归属与 NewAPI 传输错误

`codex_session_identity_conflict` 是本地账号级会话登记冲突，原始 ID 的归属检查可能与按用户隔离的出站映射发生冲突。宽松模式的处理见 [会话连续性](session-continuity.md)。它与时间容限、后台窗口容量和 BPS HTTP 400 不同。

NewAPI `do_request_failed` 表示 HTTP 请求未正常取得响应，不能由列表文字判定为模型故障。新日志在 `admin_info.upstream_error` 中保留本地传输分类、耗时与请求长度，source 为 `newapi_transport`；客户端仍收到通用错误。不记录 URL、请求头、请求体或原始错误字符串中的凭据。
