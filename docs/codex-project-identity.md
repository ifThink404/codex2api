# 项目 ID 的账号级双向映射

已识别项目 ID 使用独立的 `project-account-v1` 映射：客户端原值 a → 上游 UUID 别名 b → 客户端原值 a。映射范围包含调用方归属和所选账号（账号记录及实际账号身份），不包含线程、窗口或切号代数。同一用户/账号跨线程、重试、重启和 A→B→A 切号保持同一别名；另一用户或账号使用不同别名。

## 识别与范围

- 在项目元数据被清理前，识别有效 UUID 格式的 `project_id`、`projectId`、`workspace_id`、`workspaceId`、`X-Codex-Project-Id`、`X-Codex-Workspace-Id`，以及明确的 `project.id`、`projects[].id`、项目字段和 `project ID` / `项目 ID` 文本标注。支持对象、数组和 JSON 编码的字符串，UUIDv4/v7 都可登记。项目字段和工作区字段携带同一 UUID 时复用同一别名，不凭空把两个不同 UUID 合并。
- 登记归属于调用方。登记后，仅含这个 UUID 的后续输入也能找到原映射；未登记、没有项目语义的其他 UUID 原样保留。不会将任意 UUID 都认作项目。
- 在 `input`、`instructions`、`tools`、`additional_tools`、`tool_choice`、`text`、公开 API `metadata`、`prompt.variables` 中改写已登记 UUID。包括工具参数/结果、嵌套 JSON、对象键、路径中的完整 UUID、JSON Unicode 转义。保留未修改字节、数字精度和既有重复业务键；若改写造成不同对象键碰撞，停止请求。
- 已识别的协议项目/工作区元数据改为保留账号别名。读取当前请求的可信快照，旧清理仍先移除未受信任原值，最后由共同出站入口统一填回映射值；账号自定义头不能覆盖或新增原始项目身份。平铺字段保留原有兼容拼写，内嵌 turn-metadata 规范为 `project_id` / `workspace_id`，最终 `client_metadata` 保持字符串字典格式。同一字段各个出站载体必须一致，发现冲突停止发送。
- 当前帧规范元数据优先于平铺兼容字段；存在当前帧快照时不从旧握手补回缺失字段。相同载体中互相冲突的重复字段、同义拼写或多值请求头会被拒绝。重试/HTTP→WS 重复准备复用本次快照，别名不会再加密一遍。
- 未登记的嵌套控制容器和非协议顶层项目扩展继续清理，不将任意结构恢复到上游。无效项目字段（非 UUID、对象等）不原样发送。内部找根、绑定和调度使用原始入站身份，改写在选定账号后的出站副本执行。
- 不改写 `encrypted_content`、加密函数参数、签名、Attestation、认证数据和图片/音频 URL 等不透明字段。对 JSON 字符串中的同名结构也执行这个边界。
- 结构遍历上限 64 层；超过上限发现已登记值需要改写时停止处理，不直接放行该已知原值。

仅看不到项目语义的第一次裸 UUID，网关无法证明它是本地项目 ID，不会自动登记。模型对 ID 做截断、哈希或任意非约定编码后，也不属于完整 UUID 的可逆替换范围。

## 持久化和还原

`codex_project_registry` 只存调用方隔离的 HMAC 索引；原值/上游值双向记录复用 `codex_protocol_ids` 加密存储和现有密钥。别名复用持久 UUIDv7 分配机制，独立命名空间隔离会话、轮次、conversation 等标识。服务启动自动创建表，无需新增配置。需要备份现有数据库和密钥，不能只保留诊断日志替代映射表。

JSON 响应及 SSE/WS 转换后的共享响应边界恢复已登记别名。恢复验证调用方和实际账号；当前请求没有再次声明项目也能读取持久映射。重复处理已属于当前账号的别名不会把 b 再映射成另一个值。

流式按事件类型、输出项和内容索引隔离通道。UUID 或 Unicode 转义被拆开时，保留原事件，等待必要后缀后将还原文本放回原事件；不增加序号、不调整事件顺序。工具输入/参数流在需要时等待完整 JSON 后恢复，避免改坏签名等不透明字段。完成、输出项终态、全局终态及正常 EOF 会排空缓存；待发事件总量限制为 16 MiB。

项目映射使用带所有者归属的数据库上下文。直接嵌入执行器、没有持久化所有者上下文且业务正文出现明确项目 ID 时，返回 `codex_project_identity_unavailable`，不临时生成不可恢复的别名。映射读写失败同样停止处理。

## 各传输的协议边界

- HTTP Responses：正文元数据和可转发的头携带同一别名；仅声明请求头时，不因此向 API 中转正文强行加入 `client_metadata`。
- compact：上游不接受 `client_metadata`，正文仍按 compact schema 清理，映射元数据放在允许的请求头中，业务 `input` 使用相同别名。
- WS：逐帧元数据携带本次别名，仅出现在入站请求头中的项目也投影到 `response.create` 帧。保留会话握手身份时，项目/工作区别名同时纳入握手快照和连接复用校验，不能复用与当前项目冲突的旧握手；原有无状态连接池清理模式继续将逐请求字段放在帧内，不在握手中固定。
- API 中转仍遵守既有 turn-metadata 请求头能力策略；独立项目/工作区头只有客户端声明过才投影已映射值。

## 本地诊断

记录在 `upstream.outbound_identity.project_mapping`，由已有页面自动归入“本地改写诊断（不发送上游）”，与实际 HTTP 头/握手/正文快照分开。

```json
{
  "version": "project-account-v1",
  "account_id": 1695,
  "scope_hash": "调用方范围摘要",
  "changes": [{
    "original": "客户端项目 UUID",
    "outbound": "上游项目 UUID",
    "sources": ["body.input[0].content[0].text", "stream.response.output_text.delta"],
    "replaced": 1,
    "restored": 1
  }]
}
```

次数统计处理过的出现位置，流式增量和最终完整输出分别计数；元数据投影按位置去重。compact 和 WS 连接池可能进一步省略协议不支持的载体，因此次数不是最终上传字段数，以实际出站快照为准。诊断不是官方请求参数；入口和出口测试明确验证没有发送 project_mapping 或原值对照表。原值也不参与请求头投影。

## 验证

- `database/codex_project_ids_test.go`：并发稳定分配、密文持久化、重启、跨根/代数复用、用户/账号隔离。
- `proxy/codex_project_identity_test.go`：文本、嵌套 JSON/Unicode、对象键与大整数、签名排除、重复处理、切号、后续轮次和本地诊断；原生/API 中转 HTTP 与 compact 实际出站和回传。
- `proxy/codex_project_control_test.go`：项目和工作区元数据保留同一别名、对象/JSON 字符串、兼容拼写、重复字段拒绝、当前帧优先、无效值清理、头独有场景与投影幂等。
- 同一文件遍历普通 UUID 的全部 35 个分片点及 Unicode 转义形式的全部分片点，核对并行通道、事件序号、EOF、工具参数还原。
- `proxy/wsrelay/project_identity_test.go`：真实本地 WS 上游到 HTTP/SSE 和原生 WS 客户端，两轮请求验证 b 出站/a 回传及下一轮登记字段省略后的复用。
- 前端诊断测试确认 project_mapping 只归入本地诊断区，不混入实际出站快照。

测试使用 localhost 模拟上游、虚拟账号和临时数据库，不调用官方生产模型。启用映射会改变原来含项目原值的提示词字节，首次迁移的缓存前缀也会改变；后续稳定别名保持一致。
