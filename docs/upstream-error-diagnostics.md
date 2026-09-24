# 管理员上游错误诊断

上游失败先提取具体原因，再生成公开错误。客户端的错误码、状态码、重试语义保持原样；具体原因经脱敏后，通过已签名验证的 NewAPI 绑定加密传输。未绑定请求不发送保护字段，可在本地服务日志中搜索 `upstream_error_diagnostic`。

NewAPI 管理员日志的「详情」列显示具体原因，详情窗口补充原始错误码、类型、来源、阶段、传输方式、实际上游 HTTP 状态、握手状态及关联请求 ID。普通用户日志继续显示公开文案，后端会移除整个 `other.admin_info`。流中途失败也保存在对应消费日志的管理员信息内。

诊断不发送原始请求/响应全文。消息最多保留 1000 UTF-8 字节，屏蔽凭证、URL、主机和 IP；上游未提供错误消息时不能凭空补全原因。`http_status` 只代表已观测的上游 HTTP 状态；没有记录时显示「未观测到」，不能把网关生成的 500/502/504 当成上游状态。

HTTP 原因提取兼容 `error.message`、顶层 `message`、字符串形式的 `error` / `detail`、JSON 字符串及纯文本。此前字符串形式的 `detail` / `error` 会漏采集，导致管理员也只看到 `upstream_500` 通用提示；修复仅影响新请求，无法恢复历史未保存的内容。任意 JSON 对象或数组不会作为整段响应正文传出。

## 部署

先更新 NewAPI 接收端，再更新 codex2api。继续使用现有 NewAPI 签名绑定和共享密钥，要求请求身份与渠道元数据都已验证；无需新增密钥或数据库迁移。只更新 NewAPI 无法恢复被旧 codex2api 隐藏的原因。已存在的历史日志也不会被补写。

## 协议 v1

- HTTP 错误：`X-Codex2API-Error-Diagnostic` 响应头。
- SSE 错误：紧邻错误事件之前的 `: codex2api_error <envelope>` 注释。和失败事件一起缓冲，丢弃的重试不能提前发送。
- WebSocket：错误对象的 `details.codex2api_error`（包括 `response.failed.response.error` 和扁平 `type:error`）。
- 接收端移除保护载体，仅在对应 HTTP/协议失败时记录通过验证的诊断。成功事件不记录；渠道切换和重试使用独立记录器。
- Envelope：`v1.` + Base64URL(nonce + AES-256-GCM ciphertext + tag)，不含填充字符。
- 密钥：HMAC-SHA256(绑定密钥, `codex2api:error-diagnostic:v1`)。
- AAD：domain、NewAPI request_id、user_id、platform，以换行连接。
- 明文固定 2048 字节；前两字节为大端 JSON 字节长度，其后为 JSON 与零填充。
- 校验 request_id、channel_id、身份认证及签发时间（过去 60 秒、未来 10 秒），不把诊断当成重试建议或风控决策。
- 跨仓库固定测试报文：`proxy/testdata/upstream_error_diagnostic_v1.txt` 对应 NewAPI 的 `relay/channel/testdata/upstream_error_diagnostic_v1.txt`。
