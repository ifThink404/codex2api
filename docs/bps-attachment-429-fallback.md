# BPS 附件限流：账号级两小时回退

适用分支：sever。实现日期：2026-09-28。

## 触发范围

默认启用。只有 `/attachments` 上传产生的 `bpsAttachmentUploadError`、阶段 `http`、HTTP 状态 429，且明确匹配上传限流原因时触发：

- 专用错误码 `upload_rate_limit_exceeded`、`attachment_rate_limit_exceeded`。
- 错误码为空或 `rate_limit_exceeded`，消息精确匹配 `429: Rate limit exceeded`、`Rate limit exceeded`、`Upload rate limit exceeded`、`Attachment upload rate limit exceeded`（忽略大小写和末尾句号）。
- 错误类型限于空、`server_error`、`rate_limit_error`；已有安全策略、硬停止判断优先。

模型推理 `/responses` 的 429、Token/RPM 限流、额度不足、账号停用、安全策略、未知 429、HTML 网关错误、401/403/5xx 不会触发载体切换，保留原来的错误与重试处理。不能仅凭状态码 429 切换。

## 状态与 ID 优先级

1. 已有直接文件引用保持原逻辑；原始内容命中本地或共享的成功句柄缓存时，始终优先使用 `file_id`。
2. 没有可用句柄时先正常上传。仅上述上传限流会让本次请求就地改用回退载体，并记录两小时截止时间。
3. 两小时内，未命中的附件不再调用上传接口。已经有效的 ID 仍然复用；成功句柄缓存的原有 TTL 和失效检测不变。不会把内联结果、错误或假 ID 写进成功句柄缓存。
4. 到期后的第一个实际缓存未命中触发恢复探测，不是定时上传。只放行一个探测，其余并发请求继续回退；有共享缓存时通过租约协调实例。
5. 探测成功，恢复正常上传；再次出现同类上传限流，重新计时两小时。其他错误仍按原错误处理，不冒充上传限流。

状态按本地账号记录 ID 和上游账号标识组合隔离，不按 task/turn，不把同工作区的不同账号合并。已有上传限流不会再因为本次可用的附件回退而要求换号。原有推理 429 的重试策略不变。

两小时窗口保存在有界进程状态，并在配置了共享运行态缓存时同步。共享缓存可用时，服务重启后重新读取截止时间；仅使用进程内缓存时，重启会丢失该窗口。缓存故障保留本进程已观察到的限流状态；此时不保证跨实例协调。

需要回滚时设置 `CODEX_BPS_ATTACHMENT_429_FALLBACK=off`（也接受 `false`、`0`），恢复旧上传失败/重试行为。旧行为的测试显式使用此配置，新功能测试默认开启。

## 文件载体

| 原始内容 | 无可用 ID 且处于回退窗口时 |
|---|---|
| PNG/JPEG/GIF/WebP 图片 | `function_call_output.output` 中的 `input_image.image_url`；缺少 detail 时补 auto，显式 detail 保留 |
| 已有 HTTP(S) 文件 URL，包括 PDF | 保留 `input_file.file_url`；如果同一文件同时提供原始数据和 URL，回退时优先使用这个 URL |
| TXT/Markdown/JSON/JSONL/CSV/TSV/日志/代码/XML 等文本 | 完整文字转换为 `input_text`；支持 UTF-8 和带 BOM 的 UTF-16，不按猜测编码替换乱码 |
| HTML | 提取可见文字，排除脚本、样式；不是网页截图，也不下载外部图片 |
| DOCX/DOCM | 提取正文、表格文字、页眉页脚、脚注尾注，并携带内嵌图片；不执行宏。普通原生提取不保留精确版式 |
| XLSX/XLSM | 按工作簿顺序提取全部工作表，包括隐藏表；保留单元格地址、原始缓存值和公式；不截成前若干行。含图表、图片或嵌入对象时补充转换后的页面 |
| ODT | 提取 XML 内容和内嵌图片；需要渲染的图形使用转换器 |
| ODS/ODP | LibreOffice 转换；ODS 转 XLSX 后提取全部单元格，ODP 转为页面 |
| 旧 DOC/DOT、XLS、PPT/PPS、RTF、PPTX/PPTM 等 | LibreOffice 转换。XLS 先转 XLSX 读取全部单元格；其他转换为 PDF 后提取文字和页面图像 |
| 没有远程 URL 的本地 PDF | Poppler 提取文字并渲染全部页面，保留扫描页和图表；页面图像采用上述图片载体 |
| 损坏、加密、未知二进制、无法可靠转换、超限文件 | 明确返回 `attachment_fallback_unavailable`，不伪造成功、不静默截断、不把二进制或 Base64 当正文 |

工具结果中的文件 URL/ID 放入带来源说明的附件消息。真实工具调用和结果的类型、call_id、顺序保留。不能直接接受内联图片的用户消息或 custom 工具结果，仅把图片移到明确标注来源的适配器函数调用/结果；在一批真实工具结果结束后插入。已支持图片的 function 结果直接保留图片。

不会把服务器私有附件自动公开托管来制造 URL。仅复用调用方已提供的 URL；没有 URL 的文件在本地转换。

## 部署与资源

Dockerfile 已加入 `poppler-utils`、LibreOffice Writer/Calc/Impress 和 Noto 字体。需要重新构建镜像才能获得这些依赖。只更新 Go 二进制的部署需自行安装 `libreoffice`、`pdfinfo`、`pdftotext`、`pdftoppm`。没有转换器时，文本/DOCX/纯数据 XLSX 仍可原生解析；依赖转换器的格式明确报错。镜像体积会增加。

转换过程不走 shell，不使用文件名构造命令。每份文件使用独立私有临时目录和 LibreOffice 配置，关闭宏和活动 OLE/DDE，以及 Writer/Calc 的自动链接更新；结束或失败后清理临时文件。Linux 取消时终止转换器进程组。

保护上限：单文件解码/ZIP 展开 32 MiB，ZIP 最多 4,096 项，提取文本最多 8 MiB，本地 PDF 最多 32 页，DOCX/ODF 内嵌图片最多 32 张，最终请求仍受既有请求大小上限限制。达到上限即失败，不假装完成。转换器实例并发 2，超额等待且支持取消；每次转换最多 60 秒。PDF 按页渲染、逐页计入输出预算。这里不是操作系统 RSS 硬限制。

该回退省掉独立上传调用，但内联图片/正文仍随每次请求发送。普通解析在请求内完成，不把转换后的正文持久化存储。复杂格式反复回退会有 CPU 开销；不能据此承诺固定 RPM 或固定 Token 节省。

## 诊断与验证

时间线增加 `attachment_fallback_until_ms`、`attachment_fallback_reason`、`attachment_fallback_transitions`、`attachment_recovery_probes`、`attachment_fallback_images`、`attachment_fallback_files`。图片细节使用 `upload_429_inline_fallback`；文件细节使用 `upload_429_fallback_<format>`。不向诊断写入正文、原文件名、URL、内容摘要或文件句柄。

上传端的真实 429 仍保留在上传时序诊断中，成功回退不伪造一次新的失败推理或额外计费。最终模型 usage 和既有计费投影逻辑保持不变。

测试覆盖：两小时循环、ID 命中优先、同账号单恢复探测、跨实例租约和重载、不同账号隔离、迟到成功不清除新限流、严格错误分类、原工具历史与混合附件、DOCX 文字/脚注/图片、XLSX 隐藏表/公式、文件 URL、转换器取消/页数上限/临时清理，以及旧模式的重试兼容。转换器边界测试使用受控替身；部署镜像内的 LibreOffice/Poppler 实际转换仍需环境验证。

最终验证：`go test ./... -count=1 -timeout=10m` 全量通过，相关 BPS 回归再次通过；Linux amd64 构建通过。使用受控上传 429 加真实 BPS 推理的两次端到端测试，图片、TXT、DOCX 的随机标记全部准确返回，只有第一次发生上传尝试。这个测试验证生产转换链路，不代表本轮向真实上传接口制造了 429，也不证明所有文档格式的上游兼容性。
