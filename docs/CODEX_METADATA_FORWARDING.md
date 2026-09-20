# Codex 请求标识转发与 WebSocket 复用

## 收敛边界

| 模式 | 安装标识 | 线程、父线程与 fork 引用 | 窗口标识 |
| --- | --- | --- | --- |
| off | 保留 | 保留 | 保留客户端值 |
| device | 使用账号持久化设备身份 | 保留 | 保留客户端值 |
| session | 使用账号持久化设备身份 | 相同原始线程使用同一账号内映射，无论出现在自身、parent 还是 fork 字段 | 映射后的线程 ID + 原窗口序号 |
| full | 使用账号持久化设备身份 | 保持既有单线程折叠语义，引用也指向折叠后的线程 | 折叠后的线程 ID + 原窗口序号 |

独立 `X-Codex-Installation-Id` 仍不凭空生成。HTTP 转发 `X-Codex-Window-Id`；仅设备模式不改写窗口身份。`Session-Id` 与 `prompt_cache_key` 的既有缓存隔离策略不变；账号自定义请求头仍具有覆盖优先级，但不能绕过出站项目标识清理。

## 出站项目标识映射

所有指纹模式（包括 `off`）的项目标识均由[账号级双向映射](codex-project-identity.md)处理。有效 UUID 格式的 `project_id`、`projectId`、`workspace_id`、`workspaceId` 改为保留账号别名，覆盖 `client_metadata` 平铺键、内嵌 `x-codex-turn-metadata`（兼容 `x_codex_turn_metadata`，支持 JSON 字符串和对象）及可转发的 `X-Codex-Turn-Metadata` 头。客户端声明过的独立 `X-Codex-Project-Id`、`X-Codex-Workspace-Id` 使用同一可信别名；账号自定义头不能覆盖或重新注入原值。

映射覆盖原生 HTTP、WS 当前请求帧、compact 与 Responses 中转账号出站。compact 不支持正文 `client_metadata`，映射后的元数据仅投影到允许的头。WS 保留项目握手身份时将其纳入连接复用校验，无状态连接池仍使用逐帧元数据。内部选号、找根、绑定继续使用原始入站身份；本地项目管理 ID、根会话、线程和窗口序号不受影响。`workspaces` 和正文工作目录沿用既有行为，API 作用域头 `OpenAI-Project` 不是本地项目 UUID。

旧删除函数保留为防御边界：先清理未受信任值，最终仅由本次持久化映射填回别名。无效项目值、未知控制嵌套容器、非协议顶层扩展继续移除。WS 每帧独立取快照，不把旧握手中当前帧缺失的项目字段补回。

正文、历史、工具内容中的已登记 UUID 与协议元数据复用同一映射：a → b → a；原值/别名对照仅写入本地改写诊断。未知普通 UUID 和不透明签名/密文不纳入该改写。

## 每次请求的原始快照

出站处理先解析当前请求的 `client_metadata`，其内嵌 `x-codex-turn-metadata` 优先于旧 WS 握手元数据。存在当前帧快照时，不从旧握手补回当前帧已缺失的父线程、子代理、memory 等标记；没有帧快照的旧客户端仍可使用请求头投影。

`NewCodexFingerprint` 在改写前固定目标身份，`ApplyHeaders` 与 `ApplyBody` 复用同一快照。重复应用该快照不再次哈希 parent、fork 或 context window。HTTP 重试使用已经定稿的请求字节；WS 不再在共用执行入口和帧装配后分别对同一个请求体收敛两次。

新请求必须创建新快照，不得以已收敛的输出创建另一份原始快照。不得把快照或请求头映射存入共享连接，作为下一请求的元数据来源。

### 同一请求中的元数据冲突

HTTP、compact 和 WS 共用 `NormalizeCodexRequestMetadata`：当前正文的内嵌 `x-codex-turn-metadata` 是已声明字段的权威来源。嵌套字段与已存在的平铺同名字段或兼容别名冲突时，同步平铺值；嵌套明确提供空字符串或 null 时移除对应平铺值，不回填旧身份。覆盖 session/thread/window、轮次、父级/fork、设备及请求类别等已知元数据。字符串和对象载体均支持，不重排嵌套快照，不触碰 input、tools 或其他业务内容。

嵌套未声明的字段仍保留原有兼容语义，不猜测缺失身份，也不无条件新增平铺字段。没有有效嵌套快照的旧格式不做这项归一化。独立 `x-client-request-id` 与不透明 `x-codex-turn-state` 不按 thread_id 重写。归一化在指纹目标解析前执行，并在改写后统一别名；不改变设备收敛模式、缓存隔离键或本地账号粘性。

## WebSocket 连接复用

握手头在建连后无法更新。可复用连接不在握手中冻结窗口、线程、轮次元数据、父线程、子代理、memory 或 turn-state 等请求级标识，而将当前值放入每个 `response.create` 帧。这里握手中缺少窗口头是有意隔离，与 HTTP 漏转不同。

连接池仍按原有账号、路由和线程通道复用，不因轮次或窗口序号变化逐请求重连。`previous_response_id` 的优先连接复用额外校验原始请求通道，不能仅凭相同账号和 API Key 跳入另一显式线程。overflow 连接使用原始通道作为续链作用域；无会话槽位池使用其稳定池作用域。

正常读到响应终止事件后才归还连接；未完成、取消、断连或写出失败仍沿用现有销毁策略，避免残留响应进入下一请求。

`response.incomplete` 与 completed 一样结束本轮读取和读租约，保留原始状态、输出与 usage，并在存在 response ID 时登记原连接续链归属；不能把它改成 completed 或假造完整输出。连接本地/持久响应的保护规则不变。

连接级 `websocket_connection_limit_reached` 在 `error` 与 `response.failed` 两种事件包装下统一识别，均销毁失效连接；只检查结构化错误代码，不匹配用户正文。普通 500 overload 或 429 请求错误完整收尾后仍可复用健康连接，不因状态码一概断开。该规则只决定连接去向，不允许更换账号。

## 请求诊断中的设备与客户端信息

使用统计的「请求诊断」新增「客户端与设备」分区，展示入站字段的来源，不选择一个设备 ID 覆盖其他来源：

- 安装/设备标识：`installation_id`、`installationId`、`device_id`、`deviceId`，以及已携带的 `X-Codex-Installation-Id`、`X-Installation-Id`、`X-Device-Id` 等兼容头。
- 客户端声明：User-Agent、Originator、Version、Stainless SDK 的系统/架构/运行时版本，及元数据里的客户端版本、系统和时区。未提供的字段不从 UA 推断。
- 关联信息：补记 `context_window_id`、`window_number`、`turn_started_at_unix_ms`，保留零值和类型错误标记。NewAPI 已验证签名元数据中的安装标识、平台、Token ID、渠道 ID 也列在 `signed_newapi` 下；Token ID 是数字标识，不是密钥。
- 原生 Messages 请求可记录结构化 `metadata.user_id` 中的 `device_id`、`account_uuid`、`session_id`，不记录普通业务用户字符串、邮箱或其他扩展字段。
- 「请求信息」补充方法、端点、入站传输类型、模型、上游模型、流式标记、上游是否使用 WS、请求追踪 ID、Key ID/名称和已观测到的入站/上游 UA。没有上游 UA 观测时，不把“是否改写”推断为 false。

WS 的 `headers` 是握手来源，`client_metadata` 是当前帧来源。日志按来源保留差异，当前帧不从上一帧补设备信息。这些是客户端声明，不等于 Codex2API 收敛后的账号设备身份，也不会成为新的认证或选号依据。

诊断仅增加白名单采集与展示，不改动转发、指纹收敛、项目字段删除、窗口或计费逻辑。不采集正文/工具参数/工作目录，不额外读取请求 Body 或查询数据库、Redis、远端；复用现有元数据局部解析与 UA 审计结果。采集时文本限长脱敏，UUID 保留，自定义设备身份以 `hash:` 摘要显示。取消用量诊断原来的 12 KiB 总大小限制，已采集内容完整保存，详情读取和导出不再因为总大小而丢弃字段。

服务错误同步保存精简 `client_info`，最多 24 个来源字段、约 4 KiB；沿用有界异步写入。新字段仅对升级后的采集生效，历史缺失数据不补猜。

## 兼容性与验证

统一线程映射保留已有主线程的 v1 派生规则；此前采用独立 v2 命名空间的子线程会在升级后得到新的上游线程 ID。不能承诺跨升级保留旧 `previous_response_id` 续链。账号持久化设备 UUID 不变，完整收敛仍折叠为一个线程，并未改为保留父子线程区别。

回归测试覆盖四种模式、自身/父级/多级子线程/fork 引用、头体一致性、快照重复应用、可选字段清除、HTTP/compact 以及实际本地 WebSocket 服务器上的显式会话和无会话连接复用。使用假凭据与本地模拟上游，不代表对真实上游所有私有协议行为的保证。
