<!-- /autoplan restore point: /Users/thesong/.gstack/projects/james-6-23-codex2api/release-netcup-prompt-guard-fpfix-20260720-autoplan-restore-20260722-161345.md -->

# Codex2API V1 Prompt Guard 性能与协议覆盖修复计划

> 状态：本轮已授权的本地实现与自动化验证已完成；核心阶段 A–D、F、未知 Adapter 审计和官方应用模板闭合解析均已落地。Prompt 管理页面已收敛为审核策略与外部能力开关，不再暴露灰度发布、性能预算、缓存、队列、抽样与熔断等运行时实现参数；旧数据库字段和后端 JSON 契约继续保留，不做静默迁移。全量 Go 测试、`go vet`、PromptFilter/Proxy race、前端测试/类型检查/生产构建，以及 V1 协议 × 场景 × 300、50 个合成用户的并发矩阵均通过。Session 有序异步写入仍停在 CAS 接口授权边界；Guardian/审批任务在缺少可信任务来源签名时继续按 CurrentUser 审核；运维文档、官方基线 benchstat 对照与人工客户端验收仍待后续执行。尚未提交、推送或部署。

## 0. 任务合同

| 项目 | 锁定值 |
|---|---|
| 唯一代码仓库 | `/Users/thesong/Project/08_learn/codex2api` |
| 工作分支 | `release/netcup-prompt-guard-fpfix-20260720` |
| 计划起点 | `5a60ac8` |
| 官方性能基线 | `fd0adea`（官方 `v2.5.9`） |
| 协议范围 | 仅 V1 中实际选择模型账号并调用上游的入口 |
| 当前授权 | 用户已确认在 Codex2API 当前分支内实施本计划，并进行本地、离线验证 |
| 编码授权 | 已获得；不得越过 Session CAS 接口扩展的单独确认边界 |
| Git 授权 | 不得 commit、push、创建 tag、release 或 PR |
| 远端授权 | 不得 SSH、拉取新增生产数据、调用真实上游账号或改动生产配置 |
| 跨仓库授权 | 不得修改 NewAPI 或其他仓库 |

本计划中的“生产日志回放”只指已经落地到本地、完成脱敏的固定样本。任何新增生产访问、真实账号调用、本地 commit 或跨仓库修改，都必须单独取得用户确认，不能从“测试”“兼容”或“修复”中推导授权。

## 1. 任务目标

在不改变 Codex2API 现有 V1 对外协议的前提下，修复 Prompt Guard 对首字延迟、内存和大请求稳定性的影响，补齐遗漏的 V1 模型入口，并让所有同步拦截只针对用户可控且会实际发送给模型的文本。

最终结果必须同时满足：

1. 过滤关闭时保持官方 `v2.5.9` 的正文跳过优化，不复制、解析、散列或扫描请求正文。
2. 过滤开启时只同步审核当前用户可控的模型输入；历史、系统、工具输出和附件上下文默认不直接拦截。
3. HTTP 每请求、Responses WebSocket 每个 `response.create`、Realtime 每个实际触发上游的逻辑回合最多计算一次正文摘要；WebSocket 身份只在连接握手时验证一次。
4. 审计日志、Session 写入和外部扩展不得阻塞普通请求的首字链路。
5. `/v1/alpha/search`、Realtime、图片 JSON/multipart 等 V1 入口使用一致的安全语义。

## 2. 已确认前提

- 仅增强 V1。不得新增、迁移或修改任何 V2 路由、配置、类型或文档。
- 本轮先完成本地代码和测试，不部署生产，不提交 PR。
- 本轮仅授权修改 Codex2API；NewAPI 只作为协议边界和离线联调对象，不修改其源码、数据库或运行配置。
- 首次生产灰度继续保持账号封禁、IP 黑名单和自动处罚关闭。
- 规则命中不能只依赖总分直接终局拦截；只有当前用户 Prompt 的明确高置信命中才允许同步阻断。
- 系统提示、开发者提示、应用上下文、历史消息和工具输出默认不参与终局拦截。
- 辅助来源只允许 `off` 或 `shadow`；即使配置值为 `warn`，也只能产生非阻断提醒。辅助来源的 `enforce` 必须被拒绝或归一化为 `shadow`。
- 不引入新的独立语义模型、附件解析或情报服务；只预留已有接口。

## 3. 不在本轮范围

- NewAPI 生产部署、重启或数据库迁移。
- NewAPI 源码、管理页面、配置、数据库或部署脚本修改。
- Codex2API 生产部署、蓝绿切换或灰度开关调整。
- 自动封号、自动封 IP、处罚阈值策略调整。
- V2 协议兼容层或 V1 到 V2 的迁移。
- 新增第三方 LLM Guard 服务。
- 重写整个代理、账号调度或上游流式传输架构。
- 为了统一代码而改动与 Prompt Guard 无直接关系的官方模块。
- 新增持久化 WAL、审计目录、数据库表或其他运维基础设施。

发现上述需求时必须停止当前工作，单独列项并取得用户确认，不能顺手实现。

## 4. 不可破坏的行为

| 行为 | 约束 |
|---|---|
| V1 请求与响应格式 | 不得改变字段、状态码结构、SSE 事件或 WebSocket 帧格式 |
| 官方关闭态快速路径 | `prompt_filter_enabled=false` 且 Sidecar 不需要正文时，必须在正文构建前返回 |
| 上游流式转发 | 审计写库和 Session 更新不得延后首字 |
| NewAPI V1 签名 | 现有 canonical string、请求头和 HMAC 格式不变 |
| 默认处罚状态 | 账号封禁、IP 封禁继续关闭 |
| 日志隐私 | 不记录 Authorization、API Key、Cookie、完整请求体或未经脱敏的工具上下文 |
| 官方代码 | `api/validation.go`、`proxy/translator.go` 的官方性能优化不得回退 |
| 辅助上下文 | History/System/Developer/Tool/Attachment/Session 不得形成同步终局拦截 |
| 未知字段 | 管理页面修改已知配置时不得丢弃尚未识别的高级配置字段 |

任何阶段一旦触及 V2、NewAPI 仓库、生产或远端、禁止文件、V1 对外格式/状态码/SSE/WS 帧、账号调度、持久化 WAL、数据库 schema 或官方关闭态优化，必须立即停止并重新确认；不得以测试修复、抽象统一或兼容为理由扩大范围。

## 5. 目标架构

```text
V1 HTTP / WebSocket 帧
        │
        ▼
PromptRequestContext（每请求/每帧一个）
  ├─ 规范化后的配置快照
  ├─ 原始端点与协议
  ├─ 懒计算且只计算一次的 Body SHA-256
  └─ 一次性 NewAPI 验证结果（HTTP/连接级）
        │
        ▼
V1 Prompt Adapter Registry
  ├─ Responses / Compact
  ├─ Chat Completions
  ├─ Messages
  ├─ Images JSON / multipart
  ├─ Responses WebSocket / Realtime
  └─ Alpha Search
        │
        ▼
Prompt Budget + Guard Pipeline
  ├─ 当前用户模型输入：同步审核
  └─ 辅助上下文：默认 Shadow、有界异步
        │
        ├──────────────► 上游请求与首字转发
        │
        └─ 有界审计队列 / Session 后置写入
```

`PromptRequestContext` 不能作为进程级全局状态。HTTP 每个请求创建一次。WebSocket 将连接级已验证身份、原始事件上下文和逻辑回合上下文分离：身份只在握手时验证一次；Responses WS 的每个 `response.create` 是一个逻辑请求；Realtime 先把有界 `conversation.item.create` 用户文本放入待提交回合，仅在 `response.create` 真正触发上游时组装和审核，随后清空或推进状态。不得把每个增量/状态帧独立判为完整 Prompt，也不得跨逻辑回合复用摘要或证据。

统一适配器输出合同：

```go
type AdaptedPrompt struct {
    Segments       []Segment
    RequestedModel string
    EffectiveModel string
    Endpoint       string
    Protocol       string
    Transport      string
    Complete       bool
    Truncated      bool
}
```

Adapter 只提取审核输入，不重写、不重新序列化原请求。无法识别的新结构必须放行并产生 `adapter_unclassified` 审计，不能退化为扫描完整 JSON、工具上下文或应用上下文。当前用户片段和预算始终优先，不能被前置历史片段耗尽。

Codex 内部任务不能只靠前缀、User-Agent、客户端 metadata 或 NewAPI 对客户端正文的转发签名获得免检身份。当前实现只对官方 checkpoint、compaction summary、memory stage-one 和 ambient safety 做闭合解析：固定 boilerplate 不计分，所有动态字段仍作为 `ApplicationCandidate` 同步审核；危险动态内容可以阻断，但由多层策略保证不产生 strike/ban。任何模板漂移、重复/缺失分隔符或尾随载荷都回退 CurrentUser。Guardian/审批任务是多 content-item 状态机，本轮未在缺少可信来源证明时放宽。

## 6. 实施阶段

### 阶段 0：特征测试与固定基线

目标：先用测试锁住现有正确行为和已知缺陷，再改热路径。

任务：

- 固定官方 `fd0adea`、计划起点 `5a60ac8`、Go 版本、机器信息和基准命令。
- 先补“关闭态零额外正文分配”“重复 SHA-256”“逐协议 CurrentUser 映射”“Alpha Search 选号前拦截”“未知配置字段往返”等失败特征测试。
- 建立脱敏生产误杀语料与明确违规 CurrentUser 语料的不可变 fixture；fixture 不包含密钥、Cookie、完整工具上下文或可执行攻击细节。
- 基准和协议 fixture 先独立落地，后续每个阶段都在相同基线上比较。

停止条件：

- 如果 fixture 来源需要重新访问生产或真实上游，停止并请求授权。
- 如果无法复现已知重复散列、正文复制或协议绕过，先解释差异，不得直接修改代码。

### 阶段 A：关闭态与请求级上下文

目标：消除无条件正文复制和重复 SHA-256。

任务：

- 调整 `readRawRequestBody`，不再无条件复制 `ingress_raw_body`。
- 只有签名校验确实开启且后续代码会改写正文时，才保存一次原始正文副本。
- 新增请求级安全上下文，集中保存配置、正文摘要和 NewAPI 验证结果。
- 正文摘要以 `[32]byte` 保存在上下文中，直接基于唯一请求缓冲区懒计算；只在签名或日志确实需要时转为十六进制字符串，不得为了摘要再复制正文或重复编码。
- `verifyNewAPIPolicyContext` 在 NewAPI 关闭时立即返回，不读取或散列正文。
- 合并 overrides、rollout、Session、风险和审计元数据的重复验证调用。
- 配置在请求入口读取、规范化一次，后续只传递不可变快照。
- 配置快照中的 map/slice 不得在热更新时原地修改；请求与已排队任务必须继续使用其进入时的版本和指纹。
- 共享正文缓冲区为只读。任何转换器确需修改字节时，只能在明确的 mutation boundary 克隆一次，不得隐式就地修改原始缓冲区。

停止条件：

- 如果必须修改官方 `api/validation.go` 或 `proxy/translator.go`，停止并重新设计。
- 如果 WebSocket 需要跨帧共享正文摘要，停止；摘要只能按帧保存。
- 如果实现需要重复读取或复制正文、改变 NewAPI canonical string/HMAC 格式，停止并重新设计。

### 阶段 B：V1 协议适配与语义一致性

目标：所有真实调用上游模型的 V1 入口都经过同一条 Prompt Guard 语义链。

编码前必须先形成逐协议字段映射。同步扫描只能来自适配器明确产出的 `CurrentUser` segment；不得把整个序列化请求、整个 `messages/input` 数组或 unknown fallback 当作同步扫描输入。多轮协议中的 `CurrentUser` 是本次请求最后一个当前用户回合，既有 user-role 回合归入 `History`。

逐协议最低映射合同：

| 入口 | CurrentUser | 必须排除或降为辅助来源 |
|---|---|---|
| Responses / Compact | 本次最后一个 user message 的 `input_text`，或直接 string input | `function_call_output`、computer/tool output、旧 user 回合、instructions |
| Chat Completions | 最后一个 `role=user` 的文本 content parts | `role=tool`、assistant tool calls、image/audio refs、旧 user 回合 |
| Messages | 最后一个 `role=user` 中 `type=text` 的内容块 | 同一 user message 内的 `tool_result`、image/document、旧 user 回合 |
| Images JSON / multipart | `prompt` 与 `style` 组成的逻辑当前输入 | 文件内容、远程图片、元数据 |
| `/v1/images/jobs` | `prompt` 与 `style`，在远程图片抓取和任务入队前 | input images 与后台任务上下文 |
| Alpha Search | `commands.search_query[].q` | 其他命令元数据 |
| Responses WS | 当前 `response.create` 帧内最后一个新 user 回合的 `input_text` | function/tool output、旧 user 回合、应用说明 |
| Realtime | 自上次成功提交或取消后，由有界 `conversation.item.create` 累积并在 `response.create` 提交的最后一个新 user 回合；若 `response.create.response.input` 显式提供输入则按 Responses 规则解析 | session instructions、tool output、已提交历史、未触发上游的状态帧 |

同一当前用户回合的多个文本 content part 可按原顺序组成一个逻辑 CurrentUser 文本，但不得混入 tool result、附件或历史。若本次请求只有工具结果而没有新用户文本，则 CurrentUser 为空，工具结果只进入 Shadow。

任务：

- 为 Responses、Compact、Chat、Messages、Images、Image Jobs、WebSocket、Realtime、Alpha Search 建立显式适配器。
- `/v1/alpha/search` 只提取 `commands.search_query[].q`，在账号选择前完成审核。
- Realtime 保留内部 Responses 转译，但审计端点记录为 `/v1/realtime`。
- 图片 JSON 和 multipart 将 `prompt`、`style` 按相同方式标记为用户可控模型输入。
- multipart 不假设 `prompt` part 位于文件之前：解析器必须限制总正文、part 数、单个文本字段和临时缓冲；允许为保持原请求而进行有界临时落盘，但阻断时必须清理，且在审核完成前不得解码图片、转换 Base64、抓取远程资源、选择账号、获取并发槽或调用上游。
- Responses WS 在每个完整 `response.create` 上审核；Realtime 维护有界待提交回合，在 `response.create` 前不形成终局判定，成功提交、取消、错误或连接关闭时按状态机清理，禁止跨回合累加证据。
- 保留原始 endpoint、protocol、provider 用于审计，不用内部转译结果覆盖它们。
- `/v1/images/jobs` 现有 Prompt Guard 必须迁移到统一 Adapter/Guard 语义，并提前到远程图片抓取、并发槽获取、任务落库和后台任务启动之前。
- Models、token count、健康检查、管理接口等不选择模型账号且不转发 Prompt 的 V1 端点不在覆盖口径中。

停止条件：

- 不得为了适配器抽象改动 V1 请求或响应结构。
- 不得把系统、开发者或工具输出默认升级为同步终局拦截。
- 任何适配器无法明确区分当前用户输入时必须停止；不得用全请求兜底扫描掩盖映射缺失。
- 所有被拦截 JSON 入口必须证明账号选号、并发槽获取、临时文件创建、任务入库和上游调用次数为 0；multipart 被拦截时允许协议解析所需的有界临时缓冲，但必须证明临时文件已清理，且图片解码/转换、远程抓取、账号选号、并发槽、任务入库和上游调用次数均为 0。放行入口的上游调用次数必须恰好为 1。

### 阶段 C：大上下文预算

目标：限制内存和 CPU，同时避免简单截断造成明显漏检。

任务：

- 将当前每个 segment 独享的 `MaxTextLength` 改为分层总预算。
- 预算分两道独立执行：Adapter 在拼接和规范化前限制原始提取字节、segment/part 数与单字段大小；规范化器再限制累计派生视图、解码字节和输出字节。不得先对无界文本执行 NFKC、去隐形字符、编码识别或递归解码，再事后截断。
- `max_current_user_bytes` 与 `max_auxiliary_bytes` 分别同时约束其来源文本和规范化输出上限；任何解码还必须受现有 `max_decoded_bytes`、`max_encoded_blocks` 与最大展开倍率共同约束，取最小值生效。
- 当前用户输入在预算内先完成一次有界全段规范化和 enforce 扫描；分块只作为可证明等价的优化，不能改变终局判断。
- 当 CurrentUser 因同步 Envelope 预算发生破坏性裁剪、但完整逻辑输入不超过 1 MiB 硬上限时，先对完整 CurrentUser 生成一次有界规范化视图并保存结构化预检 Verdict；后续 LegacyRegex 不得再用裁剪样本重复判定同一来源。预检只保留配置/内容摘要、匹配结果和最多 16 KiB 的脱敏命中上下文，不保留完整 Prompt。
- 精确预检的规则候选使用不可变 Hint/Aho 索引筛选；Base64、Hex、gzip、zlib、URL、HTML、escape、ROT13、NFKC 和隐形字符派生视图仍受现有 decoded byte、block、展开与总视图预算约束。最终排除规则、引用/防御语境、累计评分、strict 与 terminal 统一走普通 Engine 的评分核心，不能由窗口证据单独重建。
- 历史、工具输出、Session 和附件共享辅助上下文预算。
- 增加 `max_segments`、`max_current_user_bytes`、`max_auxiliary_bytes`、`scan_chunk_bytes`、`scan_overlap_bytes`。
- 原始提取和规范化输出都按 UTF-8 字节独立计量；分块和重叠不得切断 UTF-8 码点。HTTP 原始/解压正文继续由既有 `RequestSizeLimiter` 和 `RequestBodyDecompressor` 限制，本轮不改变 V1 对外请求大小合同。
- 超过 1 MiB 精确预检硬上限的 CurrentUser 不得静默标记为“已完整审核”，也不得仅因长度或采样命中处罚用户；必须记录 `over_budget/truncated`，采样结果最多形成非终局 warning/审计，不得 terminal、strike、封号或封 IP。
- 语义审核只接收有界样本：开头、实际命中上下文和结尾。
- 只有最大跨界宽度可证明不超过 overlap 的检测器才能直接使用分块结果作 enforce；其他检测器只能 Shadow，或在有界 CurrentUser 上执行全段确认。
- 分块命中合并必须去重并限制累计分数。终局规则、恶意意图和上下文证据必须来自同一个 CurrentUser 逻辑片段，禁止跨块、跨来源累加成终局拦截。
- 超预算尾部进入异步 Shadow 时只能复制有界样本和摘要，禁止把完整尾部放入队列。

推荐默认值：

```json
{
  "max_segments": 64,
  "max_current_user_bytes": 131072,
  "max_auxiliary_bytes": 32768,
  "scan_chunk_bytes": 8192,
  "scan_overlap_bytes": 512
}
```

这些选项继续存储在兼容的高级配置 JSON 结构中，但管理页面不得展示原始 JSON 编辑器或“高级防护配置 JSON”文案，必须提供中文可视化配置。页面保存已知字段时必须保留未知字段，避免新旧版本之间往返保存导致配置丢失。

停止条件：

- 如果需要改变 V1 请求大小限制、增加新的对外错误或以超长为由直接拦截，停止并单独确认。
- 如果无法在不切断 UTF-8 码点的情况下保持有界扫描，停止并补充设计，不得退回简单字符串截断。

### 阶段 D：审计移出首字链路

目标：存储延迟或故障不能拖慢模型首字。

任务：

- 审计事件进入有界高低优先级队列，`block/warn` 优先。
- Shadow 与 block/warn 使用隔离或保留容量；低优先级满载时直接丢弃，保留容量也满时 block/warn 允许丢弃并递增关键指标、限频记录错误，但不得改变既有审核决定或回退同步写库。
- 封禁/处罚状态更新与审计日志写入解耦，但本轮不得开启处罚。
- 本轮不新增 WAL；指标仅包括队列深度、各优先级丢弃数、写入失败数和处理耗时。
- 异步任务不得持有 `gin.Context`、`http.Request`、原始 body、完整 Envelope、Header map 或可变配置指针，只复制有上限且已脱敏的证据和不可变元数据。
- 不得每条事件创建 goroutine。固定 Worker 必须恢复 panic、对存储失败使用有界退避，并在服务关闭时按固定超时排空。
- `block/warn/shadow` 分别定义证据最大长度和脱敏规则。审计中分开保存“当前用户 Prompt 预览”和“规则命中证据”，不得用工具路径或工具输出覆盖 Prompt 预览。

停止条件：

- 如果异步化需要新增数据库 schema、首次引入持久化 WAL/新目录、修改 SSE/WS writer、同步重试或队列失败时 fail-closed，立即停止并单独确认。
- 审核决定必须独立于审计存储成功；不得为了“保证日志落库”阻塞请求或首字。

### 阶段 E：Session 后置与顺序保证

目标：Session 关联保持可选、非阻断，并避免异步乱序覆盖。

任务：

- 普通请求没有明确续写特征时，不执行 Session 读取；推荐配置保持 `combine_short_fragments=false`。
- 只有已验证身份且有明确 session fingerprint 的请求才允许关联；读取最多一次且有短超时，超时后无 Session 上下文继续。
- 放行记录使用有界异步写入，Blocked 请求不得污染 Session。
- 写入携带单调版本/序号和 RequestID/EventID 幂等键；使用 CAS、版本比较或等价的按 key 顺序机制，拒绝旧写覆盖新写。
- Session 队列任务只保留有界 CurrentUser 片段和不可变身份摘要，不保留请求上下文或完整正文。

停止条件：

- 如果现有 cache 接口无法提供 CAS、版本比较或等价的顺序保证，停止并单独提出最小扩展，不得用无序 goroutine 写入代替。
- Session 读写故障不得改变当前 Prompt 的本地同步判定，也不得延迟首字。

### 阶段 F：RawConfig / EffectiveConfig 与管理页面

目标：在不增加数据库字段的前提下，同时保留未知 JSON 和提供规范化运行配置。

任务：

- 存储层保留原始 `RawConfig` 字符串；运行时从 RawConfig 解析出只读 `EffectiveConfig`。
- GET 返回 RawConfig；PUT 以字段级 merge patch 更新 RawConfig，再解析、验证并原子替换 EffectiveConfig。
- 禁止通过强类型 `AdvancedConfig` 重新序列化整份配置后覆盖 RawConfig。
- 未知字段、未来版本字段和扩展服务字段必须完成后端 `GET → 修改一个已知字段 → PUT → GET` 往返测试。
- 管理页面按第 7 节合同实现深度 patch、状态分离、旧值迁移、本地化与无障碍。

停止条件：

- 如果必须新增数据库字段或迁移才能保留 RawConfig，停止并重新确认；优先复用现有高级配置字符串。
- 如果 GET/PUT 兼容需要改变现有公开 settings 响应结构，停止并提出兼容方案。

## 7. 管理页面与配置兼容合同

本轮只扩展现有 Prompt Filter 页面，复用 `AdvancedPanel`、`CompactField`、`SwitchField`、`Select`、`DraftNumberInput`、`StateShell` 和现有 toast，不创建新页面、不增加导航、不做视觉重构，因此不需要高保真 mockup。

页面信息层级固定为：

```text
Prompt Guard
  ├─ 1. 审核模式与来源层级
  │     ├─ 当前用户：关闭 / 仅记录 / 警告 / 拦截
  │     └─ 辅助来源：关闭 / 仅记录（旧 warn 显示兼容态，不提供拦截）
  ├─ 2. 当前用户扫描预算
  │     ├─ 总预算 / 分块 / 重叠 / segment 上限
  │     └─ 超预算行为说明
  ├─ 3. 异步 Shadow 与精确缓存
  │     ├─ 推荐开关
  │     └─ 可折叠的容量、TTL、worker、queue、overflow 细节
  └─ 4. 审计与 Session
        ├─ 异步写入状态
        └─ 队列指标与非阻断说明
```

### 配置存取合同

- 前端必须保留原始 JSON 解析树，只对用户实际修改的已知 JSON Pointer 做 immutable deep patch；不得把配置反序列化为固定结构后整体重建。
- 未知顶层字段，以及 `guard`、`performance`、`layers`、`provider_profiles` 内的未知嵌套字段，保存前后必须保持语义等价。
- JSON 语法错误时，高级配置区进入只读错误态，保留原文并提供“重新加载”和“恢复推荐值”两个操作；恢复推荐值必须二次确认。
- 未知枚举值显示为“未知值（原值）”，对应控件只读，其他安全可映射字段仍可编辑；保存不得用 fallback 静默覆盖未知值。
- 配置 PUT、服务器归一化回填、规则/日志等附属刷新是三个独立状态。PUT 成功即标记“已保存”；后续刷新失败只能提示“已保存，附属数据刷新失败”，并提供独立重试。
- 服务器归一化、裁剪或兼容降级后的实际值必须回填到表单并显示说明，不能继续展示未生效的用户输入值。

### 默认值与旧配置迁移

| 场景 | 行为 |
|---|---|
| 全新安装 / 用户点击“推荐配置” | `async_shadow_auxiliary_enabled=true`、精确缓存开启、overflow=drop，CurrentUser=enforce，辅助来源为 off/shadow |
| 旧配置显式保存过某个值 | 原值优先；但已证明会把 Shadow 扫描拉回首字链路的 `overflow=sync` 例外归一化为 drop |
| 旧配置缺少 `async_shadow_auxiliary_enabled` | 保持兼容值 `false`，页面显示“旧配置未开启，建议开启”；不得升级后自动改变生产行为 |
| 辅助来源旧值 `warn` | 运行时保持非阻断，界面显示“兼容提醒（不拦截）”；用户修改时只能选关闭或仅记录 |
| 辅助来源旧值 `enforce` | 运行时归一化为 Shadow；服务端回填“已降为仅记录”，保存后持久化 Shadow，不允许形成同步阻断 |
| 未知枚举/未来版本字段 | 保留原值，不静默回退、不自动删除 |

危险性能组合不禁止管理员选择，但必须常驻提示风险并在保存前二次确认：

- `async_shadow_auxiliary_enabled=false` 且 `exact_segment_cache_enabled=false`。
- Session 同步写入或审计同步写入（若旧配置仍存在这些值）。

`shadow_overflow_mode=sync` 不再作为可选行为：旧值运行和保存时都归一化为 `drop`，页面只展示“队列满时丢弃并记录”。这是首字性能正确性保证，不提供关闭开关。

### 新字段合同

| JSON 路径 | 类型/范围 | 缺省与迁移 | 中文显示 |
|---|---|---|---|
| `guard.performance.max_segments` | 整数，1–256 | 推荐 64；旧配置缺省时只采用后端兼容默认 | 最大文本段数量 |
| `guard.performance.max_current_user_bytes` | 整数，8192–1048576 | 推荐 131072 | 当前用户同步扫描上限 |
| `guard.performance.max_auxiliary_bytes` | 整数，0–262144 | 推荐 32768 | 辅助上下文异步扫描上限 |
| `guard.performance.scan_chunk_bytes` | 整数，1024–65536 | 推荐 8192 | 单次扫描分块大小 |
| `guard.performance.scan_overlap_bytes` | 整数，64–8192 | 推荐 512 | 相邻分块重叠大小 |

交叉约束必须由前端和服务端共同校验：`overlap < chunk <= current_user_budget`，辅助预算不能超过进程级硬上限，segment 上限和队列容量必须在已定义范围。错误在字段旁显示，使用 `aria-describedby` 关联；表单无效时禁用保存，不能只弹 toast。

### 交互状态矩阵

| 状态 | 用户看到的内容 | 允许的操作 |
|---|---|---|
| 初次加载 | 页面级加载状态，不闪现默认值 | 等待或返回 |
| 非关键附属数据加载失败 | 配置仍可查看，显示局部错误 | 单独重试附属数据 |
| JSON 语法错误 | 持久错误、原配置未被覆盖 | 重新加载；确认后恢复推荐值 |
| 未知字段/枚举 | 原值和兼容说明 | 编辑其他安全字段；未知字段只读 |
| 未保存修改 | 明确 dirty 状态 | 保存、放弃；离页/刷新前确认 |
| 保存中 | 保存按钮禁用并显示进度 | 禁止重复提交 |
| 保存成功 | 回填服务端真实值和成功提示 | 继续编辑 |
| 保存成功、附属刷新失败 | “已保存，附属数据刷新失败” | 单独重试刷新 |
| 服务端校验失败 | 对应字段错误和修复建议 | 修改后重试 |
| 恢复推荐值 | 二次确认并说明会改哪些字段 | 确认或取消 |

### 本地化、响应式与无障碍

- 简体中文、繁体中文、英文资源必须同时拥有新增 key；测试必须验证 key 完整，关键枚举的 zh-TW 不得仅依赖英文 fallback。
- 中文界面不得直接显示 `off/shadow/warn/enforce/balanced/strict/drop/sync/async` 等内部值。每个枚举必须有中文名称、一句结果导向说明和推荐标记。
- label 与控件必须显式关联；帮助、错误分别使用本地化 `aria-label`、`aria-describedby` 和 `aria-live`。tooltip 必须可点击、可聚焦、可用键盘关闭，不能只靠 hover。
- 所有交互支持键盘，焦点样式可见，触控目标至少 44px。
- 在 320、375、768、1280px 验收：移动端字段单列、按钮不横溢、长中文标签不得只靠 truncate 隐藏含义，顶部标签仍可辨识和操作。

## 8. 配置边界

下列修复是内部正确性与性能保证，不提供关闭开关：

- 请求正文不重复复制。
- 每请求/每帧摘要只计算一次。
- NewAPI 关闭时不计算摘要。
- 原始端点和协议审计正确。

下列行为允许管理员配置：

- `CurrentUser` 可配置为 `off`、`shadow`、`warn` 或 `enforce`。
- `History/System/Developer/ToolArguments/ToolOutput/Attachment/Session` 只允许 `off` 或 `shadow`；输入 `warn` 时保持非阻断，输入 `enforce` 时拒绝保存或归一化为 `shadow`。
- 扫描预算和分块大小。
- Session 是否启用以及写入模式。
- 审计队列容量和工作线程；溢出行为固定为非阻断丢弃并计数。

安全默认值：

```text
CurrentUser = enforce
History/System/Developer/ToolArguments = off
ToolOutput/Attachment/Session = shadow
AsyncShadowAuxiliaryEnabled = true
ExactSegmentCacheEnabled = true
ShadowOverflowMode = drop
SessionWriteMode = async
AuditWriteMode = async
AccountBan = false
IPBlock = false
```

## 9. 代码修改边界

预计允许修改：

- `proxy/handler.go`
- `proxy/newapi_policy.go`
- `proxy/prompt_guard.go`
- `proxy/prompt_guard_extensions.go`
- `proxy/prompt_filter.go`
- `proxy/responses_ws.go`
- `proxy/realtime_ws.go`
- `proxy/codex_alpha_search.go`
- `proxy/images.go`
- `admin/image_studio_external.go`
- `admin/prompt_filter.go`
- `admin/handler.go` 中仅与高级配置 Raw/Effective 往返直接相关的代码
- `security/promptfilter/envelope.go`
- `security/promptfilter/advanced.go`
- 对应 Go 测试、管理页面配置组件和中文文档

默认禁止修改：

- `api/validation.go`
- `proxy/translator.go`
- 账号调度核心
- 上游 SSE/WebSocket writer
- 数据库主 schema；若 WAL 或队列确实需要新表，必须单独提出并确认
- 所有 V2 文件与路由

管理页面只允许扩展现有 Prompt Filter 页面，不新建导航、仪表盘或新的配置存储。新增枚举必须在简体中文、繁体中文和英文资源中有完整映射；中文界面不得直接显示 `off/shadow/warn/enforce/balanced/strict/drop/sync` 等内部值。

每次修改函数前必须运行 GitNexus impact。HIGH 或 CRITICAL 影响必须先说明覆盖的 V1 流程，再编辑。

## 10. 测试计划

### 单元测试

- NewAPI 关闭时摘要计算次数为 0。
- NewAPI 开启时 HTTP 请求摘要计算次数为 1。
- Responses WebSocket 每个完整 `response.create` 各计算一次摘要；Realtime 只有实际触发上游的 `response.create` 逻辑回合计算一次，纯状态/累积帧不被当作独立 Prompt。
- WebSocket 审计明确区分 `identity_verified=true` 与 `frame_integrity_verified=false`；同连接多回合、跨连接重复 EventID、乱序和重连均不串线。
- Realtime 覆盖 `conversation.item.create → response.create`、显式 `response.input`、仅工具结果、错误、取消和断连清理；前一回合证据不得泄漏到下一回合。
- 关闭过滤时不设置 `ingress_raw_body` 副本。
- Alpha Search 正常搜索通过，违规查询在账号选择前拦截。
- Alpha Search 被拦截时选号、Release 和上游调用均为 0。
- Realtime 审计端点为 `/v1/realtime`。
- 图片 JSON 与 multipart 对同一 `prompt/style` 得到相同结果。
- multipart 覆盖 prompt part 在文件前/后的两种顺序；阻断后临时缓冲被清理，图片解码/转换、远程抓取、选号、并发槽、任务入库和上游调用均为 0。
- 大量 segment 不能突破辅助上下文总预算。
- 原始来源文本在规范化前已受预算限制；超长 Unicode、压缩正文、Base64/Hex 多轮解码和高展开倍率输入不能让规范化 CPU、派生视图或 retained heap 越过上限。
- 多轮协议只把本次最后一个用户回合作为 `CurrentUser`，旧 user-role 回合归入 `History`。
- 跨扫描块规则通过 overlap 正确命中。
- 超预算请求产生明确审计标记但不因长度被拦截，未扫描部分只进入有界异步 Shadow。
- 审计队列、Session 队列满载和关闭时行为明确。
- 审计/Session 异步任务不包含原始 body、Header、请求上下文或完整 Envelope 引用。
- Session 同 key 反序完成时旧版本不能覆盖新版本；重复 RequestID/EventID 幂等。
- 数据库永久阻塞、Worker panic、关机排空超时不会阻塞请求或无限等待。
- 16MiB/64MiB 请求结束且队列积压时，retained heap 只受配置预算和队列字节上限约束。
- `/v1/images/jobs` 在远程输入图片抓取、并发槽、任务入库和 goroutine 启动前完成审核。
- 高级配置可视化保存后未知字段仍然存在，所有新增选项在中文界面显示中文值和解释。
- 非法 JSON 不会被默认值覆盖，未知枚举不会被保存操作改写。
- 保存成功与附属刷新失败状态分离，服务端归一化值正确回填。
- dirty/离页确认、重复提交保护、字段级错误、键盘操作和 375px 布局通过前端回归。

### 协议回归

- `/v1/responses`
- `/v1/responses/compact`
- `/v1/chat/completions`
- `/v1/messages`
- `/v1/images/generations`
- `/v1/images/edits` JSON/multipart
- `/v1/images/jobs` 与 edit job
- `/v1/alpha/search`
- `/v1/responses` WebSocket
- `/v1/realtime`

每种协议至少覆盖：正常 Prompt、明确违规 Prompt、工具输出含敏感字但当前 Prompt 正常、过滤关闭、Shadow、Warn、Enforce。

协议回归必须在独立本地配置、独立临时数据库和不占用用户当前 `3000/18095` 服务的测试端口中运行。默认使用 stub/fake upstream；未获得单独授权不得使用真实账号或生产服务。普通 CI 运行协议契约和关键并发测试；完整协议 × 场景 × 300、50 个合成 NewAPI 用户的矩阵放入独立压力测试任务，避免每次提交运行数万请求。

### 性能门槛

- 8MB 关闭态 `RequiresRequestText`/过滤短路基准保持 `0 allocs/op`；不把整个 HTTP 正文读取链路错误要求为零分配。
- 与锁定的官方 `fd0adea` 基线相比，关闭态额外 CPU 不超过 5%。
- NewAPI 关闭时不得出现正文 SHA-256。
- 短正常 Prompt 热缓存路径新增本地 CPU 目标不超过 1ms。
- 数据库延迟和 Session 写延迟不得进入首字关键路径。
- 辅助上下文保持异步和精确缓存时，不得从微秒级退化到毫秒以上的同步扫描。
- 基准同时记录 p50/p95/p99、峰值分配和峰值内存；均值达标但长尾或内存明显退化仍视为失败。
- Exact Segment Cache 的 key 必须包含检测器版本、规则/归一化指纹、规范化文本摘要、完整/截断状态和分块参数；只有检测器实际读取来源或协议时才把 provenance/protocol 加入 key。配置或规则更新后不得复用旧判定，缓存不得保留 Prompt 明文。
- 相同机器、相同 Go 版本、`GOMAXPROCS=1`、完成预热，每项至少运行 10 次并使用 benchstat 比较中位数和置信区间；未超过统计噪声视为通过。
- 过滤关闭不得新增随正文大小线性增长的额外分配；过滤开启的同步 retained memory 只能随配置预算增长，异步单项和全队列最大内存必须可计算并有测试。

### 必跑命令

```bash
npm --prefix frontend ci
npm --prefix frontend run build
go test -count=1 ./...
go test -race -count=1 ./security/promptfilter ./proxy
go vet ./...
go test -run '^$' -bench 'BenchmarkInspect|BenchmarkGuardPipeline' -benchmem ./security/promptfilter
npm --prefix frontend test
npm --prefix frontend run typecheck
npm --prefix frontend run build
```

运行前先记录 `go version`、`node --version`、`npm --version`。Stage 0 必须锁定 benchstat 版本，并保存 baseline/new 原始输出和比较结果；离线环境需要提前准备 npm 缓存和 benchstat 二进制。管理页面发生变化时，前端测试、类型检查和生产构建全部必跑。

### 执行路径覆盖图

```text
V1 HTTP
  ├─ Guard/审计均不需要正文
  │    └─ [回归] 官方关闭态：无额外复制/摘要/解析
  └─ 需要正文
       ├─ [回归] RequestContext：共享只读正文 + digest once
       ├─ Adapter
       │    ├─ [回归] 已分类 CurrentUser → 同步审核
       │    ├─ [回归] 仅 tool/session → CurrentUser 为空、辅助 Shadow
       │    └─ [回归] 未分类结构 → 放行 + adapter_unclassified
       ├─ Guard
       │    ├─ [回归] allow/warn/block
       │    ├─ [回归] over_budget/truncated
       │    └─ [回归] chunk/full-scan 等价与不跨来源累计
       ├─ [回归] block → JSON 零临时文件；multipart 仅有界暂存且清理；零解码/抓取/选号/并发槽/上游
       └─ allow → 上游一次 → 首字不等待审计/Session 写入

V1 WebSocket / Realtime
  ├─ [回归] 握手身份验证一次
  ├─ [回归] 原始事件与逻辑回合分离；每个上游触发回合 digest once
  ├─ [回归] Realtime 累积 → commit/response.create → 清理，不跨回合拼接
  ├─ [回归] identity_verified 与 frame_integrity_verified 分离
  └─ [回归] 帧乱序、重连、跨连接不串线

异步 Worker
  ├─ [回归] 有界不可变任务，无 request/body/header 引用
  ├─ [回归] queue full / DB 阻塞 / panic / shutdown timeout
  └─ [回归] Session 版本比较与幂等
```

## 11. 已有能力与复用边界

| 已有能力 | 计划处理方式 |
|---|---|
| `RequiresRequestText` 关闭态短路 | 保留并增加官方基线回归，不重写 |
| 官方 `api/validation.go` 与 `proxy/translator.go` 优化 | 完全不改，只用基准证明未被外围逻辑抵消 |
| GuardPipeline 的来源分层、审核档位和终局判定 | 复用；Adapter 只负责正确产生 segment，不另建第二套评分器 |
| 精确 segment cache 与有界 Shadow dispatcher | 收紧 key、任务所有权和溢出语义，不另建无界 goroutine 系统 |
| `TextPreview` 与 `MatchContext` 两类审计证据 | 保留分离；修复 Prompt 预览被工具路径/证据覆盖的问题 |
| NewAPI V1 HMAC、数据库/环境密钥来源 | 保持 canonical string；只消除重复散列并明确 WS 边界 |
| Prompt Filter 可视化页面与三语资源 | 在现有组件内扩展，不创建新页面或原始 JSON 编辑器 |
| 现有高级配置字符串列 | 作为 RawConfig 继续使用，不新增数据库字段 |
| `/v1/images/jobs` 现有 legacy Prompt 检查 | 迁移到统一 Adapter/Guard，并提前到远程资源处理之前 |
| SSE/WS 输出扫描 | 只做回归，除非测试证明本轮修改破坏现有语义，否则不改 writer |
| 生产误杀回归和并发测试基础 | 复用本地脱敏 fixture，新增逐协议契约、内存和异步故障测试 |

## 12. 故障模式、安全边界与可观测性

### 故障模式

| 故障 | 请求行为 | 审计/运维行为 |
|---|---|---|
| Adapter 无法识别 | 放行，不扫描完整 JSON | `adapter_unclassified`，记录 endpoint/protocol/schema 摘要 |
| 新配置无效 | 继续使用最后一次有效 EffectiveConfig | `config_invalid` 限频告警，保留 RawConfig |
| NewAPI 关闭 | 不读取、不散列正文用于身份验证 | 不记录“验证失败” |
| NewAPI 签名无效 | 视为未验证身份，不冒充真实用户 | `newapi_identity_invalid`，不记录密钥或签名原文 |
| WebSocket 后续帧 | 使用连接身份但不声称帧完整性已验证 | `identity_verified=true`、`frame_integrity_verified=false` |
| Session 读取超时 | 无 Session 上下文继续 | 计数，不影响当前 Prompt 判定 |
| Session 写队列满/乱序 | 不阻塞响应，旧版本不得覆盖新版本 | 丢弃/拒绝旧写并计数 |
| 审计队列满 | 不阻塞响应，不回退同步写库 | 按优先级丢弃、关键指标和限频错误 |
| Worker panic/数据库永久阻塞 | 不影响请求线程 | recover、超时、计数和限频日志 |
| Sidecar/附件服务超时 | 按现有 fail-open/shadow 合同继续 | 记录稳定原因码；本轮不扩大外部服务 |
| 正文超预算 | CurrentUser 预算优先，不因长度直接拦截 | `over_budget` / `truncated`，有界 Shadow 样本 |
| 规范化展开耗尽预算 | 停止继续派生，不把不完整结果伪装成完整审核 | `over_budget` / `normalization_incomplete`，仅对已完整确认的 CurrentUser 证据执行终局判定 |
| multipart Prompt 位于文件之后 | 有界暂存或流式推进至 Prompt，不提前处理图片 | 阻断后清理临时资源；清理失败限频告警但不调用上游 |
| Realtime 回合未提交或连接断开 | 不把碎片当作独立完整 Prompt，不写入可续用 Session | 丢弃有界 pending turn，仅记录不含正文的状态计数 |
| 服务关闭 | 固定超时排空 | 剩余任务丢弃并统计，不无限等待 |
| 旧版本混合运行 | 旧颜色只读高级配置 | 管理写请求只路由新颜色，避免旧版回写丢字段 |

### 安全保证边界

- 本轮保证“已支持且在预算内的 CurrentUser 输入”按显式 Adapter 审核，不承诺对未知未来协议、未解析附件或预算外全部字节实现 100% 语义检测。
- `over_budget`、`adapter_unclassified` 和 WS 帧未签名必须可见，不能伪装成“已完整审核”。
- 不同 segment、来源或请求的证据不得拼接成高分或终局命中；同一当前用户逻辑回合内的文本 part 才允许按顺序组合。
- 规则/配置热更新、缓存碰撞、队列堆积、伪造身份头、混合版本回写和审计泄漏均必须有对应回归。
- 指标、日志标签和缓存 key 不得包含 Prompt 明文、用户原始 IP、密钥、Cookie 或 Authorization。

### 稳定诊断代码

管理配置与运行诊断至少提供下列 machine-readable code，并同时给出中文问题、原因、修复动作和相关字段路径：

```text
invalid_json
unknown_enum
constraint_violation
config_conflict
adapter_unclassified
over_budget
normalization_incomplete
audit_queue_drop
session_queue_drop
newapi_identity_invalid
multipart_cleanup_failed
realtime_turn_discarded
legacy_value_normalized
worker_panic
shutdown_drain_timeout
```

这些诊断不得改变既有 V1 错误 envelope 或状态码结构。V1 对用户的阻断响应保持现有兼容格式；诊断细节进入受限审计和管理端，不向普通用户泄漏规则实现。

### 指标与展示

复用现有 Prompt Filter/Ops 页面和运行日志，不新建页面。至少展示或导出：

- 当前 RawConfig/EffectiveConfig revision 与不可逆 fingerprint，不显示配置密文。
- endpoint、protocol、adapter、source layer、同步/Shadow 判定计数。
- 正文摘要计算次数、Adapter 未分类数、超预算数。
- 审计/Session 队列深度、保留字节、各优先级丢弃、失败、处理耗时和最后错误时间。
- 首字前 Guard 各阶段耗时的 p50/p95/p99；不得把用户 ID、IP、Prompt 或 RequestID 作为高基数指标标签。

## 13. 管理员首次使用、升级与文档合同

### 管理员 Persona 与时间目标

- 主要用户：自托管 Codex2API 管理员和维护二次开发的工程师。
- 从登录管理页到“安全配置生效、正常样本通过、本地无害测试规则被拦、对应审计可见”：不超过 5 分钟、6 个动作。
- 从看到配置/签名/队列错误到定位具体字段或修复文档：不超过 2 分钟。
- 已完成蓝绿部署后的应用切回不超过 2 分钟；必要时恢复 RawConfig 不超过 5 分钟。

### 六步安全启用路径

1. 查看推荐值与当前值差异。
2. 明确显示总开关、strict terminal、二次审查 fail-closed、账号封禁和 IP 封禁的实际状态。
3. 保存配置；推荐配置不得暗中开启 strict terminal、review fail-closed、账号封禁或 IP 封禁。
4. 页面显示服务端 EffectiveConfig revision/fingerprint 和生效来源。
5. 用内置测试分别验证一条正常样本和一条本地无害测试规则样本，不调用真实上游。
6. 一键跳转到对应审计记录，确认 endpoint、层级、原因和 Prompt 预览正确。

### 热加载与来源优先级

| 配置 | 生效方式 | 优先级/说明 |
|---|---|---|
| Prompt Guard 模式、层级、预算、缓存、worker、队列逻辑上限 | 保存后对新请求立即生效 | DB RawConfig → EffectiveConfig；在途请求使用进入时快照 |
| NewAPI 数据库密钥 | 无环境变量覆盖时立即生效 | ENV > DB > 未配置；UI 只显示来源和配置状态 |
| `PROMPT_FILTER_NEWAPI_SECRET` | 进程启动时读取 | 修改环境变量后需重启；明文不得回显 |
| 处罚/封禁 | 本轮始终关闭 | 未来需单独授权，不因推荐配置启用 |

### 文档交付

实现阶段必须同步更新：

- `README.md`、`README.zh-CN.md` 的文档索引和首次配置入口。
- `docs/prompt-filter-hardening.md`：删除“首次启用 strict terminal + review fail-closed”的危险推荐，区分新安装、旧配置升级、首次 Shadow 灰度和验证后 Enforce。
- `docs/newapi-audit-integration.md`：明确 HTTP body HMAC 与 WS 连接身份/帧完整性的区别。
- `docs/CONFIGURATION.md`：字段范围、默认迁移、热加载、ENV/DB 优先级和诊断代码。
- `docs/TROUBLESHOOTING.md`：配置无效、未知字段、Adapter 未分类、队列丢弃、签名失败、首字回退排查。
- `docs/DEPLOYMENT.md`：RawConfig 备份、蓝绿混合版本写入规则、灰度和完整回滚。

旧文档不得继续把 strict terminal、review fail-closed 或自动处罚描述为首次启用推荐值。所有示例必须使用 V1，不能新增 V2 指引。

### 蓝绿与完整回滚合同

- 部署前导出 RawConfig 和数据库备份，记录旧/新镜像 digest、commit 和 EffectiveConfig fingerprint。
- 新旧颜色共库期间，旧颜色可以读取并忽略未知字段，但不得处理高级配置写请求；管理写请求必须只路由到新颜色。
- 必须验证新版本写入的 RawConfig 能被旧版本启动和读取；若旧版不能安全读取，禁止进入混合运行。
- 回滚不是只切流：切旧颜色后验证 health、正常 V1 请求、Prompt Filter 总开关和处罚状态；必要时恢复 RawConfig 快照。
- 本轮不部署，上述步骤只作为未来取得部署授权后的硬门槛。

### 维护门禁

增加路由清单契约测试：每个会选择模型账号并向上游发送 Prompt 的 V1 HTTP/WS 入口必须注册 Adapter；不含 Prompt 的入口必须显式列入 `no_prompt` allowlist。出现未分类新路由时测试失败，运行时不得扫描整包兜底。

## 14. 实施并行化与任务清单

### 依赖与并行通道

| 工作流 | 主要模块 | 依赖 |
|---|---|---|
| 基线、fixture、路由契约测试 | `security/promptfilter/`、`proxy/*_test.go`、`admin/*_test.go` | 无 |
| RequestContext 与 NewAPI 去重 | `proxy/handler.go`、`proxy/newapi_policy.go` | 基线 |
| V1 Adapter 迁移 | `proxy/`、`admin/image_studio_external.go` | RequestContext |
| 预算与扫描语义 | `security/promptfilter/` | Adapter 合同 |
| 审计异步 | `proxy/prompt_guard.go`、审计写入 | 预算输出结构 |
| Session 异步 | `proxy/prompt_guard_extensions.go`、cache | 审计 Worker 模式 |
| Raw/Effective 配置与 UI | `security/promptfilter/advanced.go`、`admin/handler.go`、`frontend/` | 字段 schema 锁定 |
| 文档与运维说明 | `README*`、`docs/` | 默认值、诊断和部署合同锁定 |

Lane A：基线 → RequestContext → NewAPI 去重 → Adapter → 预算 → 审计 → Session，必须串行，因为共享热路径和数据合同。

Lane B：UI raw-tree patch、中文资源和状态测试可在字段 schema 锁定后独立进行；与后端 RawConfig 合并前不得自行决定字段语义。

Lane C：文档和离线 fixture 可并行准备，但最终内容等待工程默认值和诊断代码确定。

不得让两个工作区同时修改 `handler.go`、`prompt_guard.go` 或 `advanced.go`；合并顺序固定为 A → 后端 RawConfig → B → C → 完整回归。

### Implementation Tasks

- [x] **T1（P1，human: ~2h / Codex: ~20min）** — 测试基线 — 固定官方基线并补已知失败特征测试。
  - Verify：关闭态、重复摘要、Alpha Search、逐协议 CurrentUser、RawConfig 往返测试先失败且原因正确。
- [x] **T2（P1，human: ~4h / Codex: ~45min）** — RequestContext — 消除正文重复复制、散列和配置重复规范化。
  - Verify：8/16/64MiB 分配、digest 次数、race 与官方基准。
- [x] **T3（P1，human: ~1d / Codex: ~2h）** — V1 Adapter — 完成 HTTP/WS/Image Jobs 显式字段映射、Realtime 逻辑回合状态机、multipart 有界暂存和路由门禁。
  - Verify：所有阻断入口零选号/零上游；multipart 阻断零解码/抓取且临时资源清理；Realtime 不跨回合；正常入口恰好调用一次。
- [x] **T4（P1，human: ~1d / Codex: ~2h）** — 扫描预算 — 实现 CurrentUser 优先、全段确认、UTF-8 安全和超预算标记。
  - Verify：规范化前后双预算、跨块、编码、Unicode、展开倍率、不同来源不累计及内存上限。
- [x] **T5（P1，human: ~1d / Codex: ~2h）** — 审计异步 — 去除请求引用并实现优先级、字节上限、故障恢复和关机排空。
  - Verify：queue full、DB 阻塞、panic、retained heap 和 Prompt 预览/证据分离。
- [ ] **T6（P1，等待授权）** — Session 异步 — 已完成明确续写门槛、短超时读取与默认关闭；CAS/版本比较和有序异步写入等待最小 cache 接口扩展授权。
  - Verify：反序完成、重复 RequestID、超时和 Blocked 请求不污染。
- [x] **T7（P1，human: ~1d / Codex: ~2h）** — 配置兼容 — 实现 RawConfig/EffectiveConfig、后端 merge patch 和未知字段往返。
  - Verify：GET→改单字段→PUT→GET 未知字段保持，非法 JSON 不覆盖最后有效配置。
- [ ] **T8（P2，核心功能完成）** — 管理页面 — 可视化预算、未知字段保留、旧值迁移和三语已完成；响应式与完整无障碍人工验收待执行。
  - Verify：dirty、部分成功、未知枚举、键盘和 320/375/768/1280px。
- [ ] **T9（P2，human: ~4h / Codex: ~45min）** — 运维文档 — 修正危险旧建议并补首次使用、诊断、蓝绿和完整回滚。
  - Verify：文档只使用 V1，首次灰度不启用处罚/strict/fail-closed。
- [ ] **T10（P1，自动化验证完成）** — 完整验证 — 全仓测试、PromptFilter/Proxy race、vet、前端 21 项测试/类型检查/构建、关键基准，以及 V1 协议 × 场景 × 300、50 个合成用户的并发矩阵已通过；官方基线 benchstat 对照与真实客户端首字验收待执行。
  - 当前机器（Apple M1 Pro）复测：关闭态 8 MiB 请求约 `37.5 ns/op`、`0 allocs/op`；缓存后的普通开发请求约 `0.208 ms/op`；启用审核的 8 MiB 请求约 `64.9 ms/op`；SSE 写入热路径约 `169 ns/event`。
  - 编码/压缩压力样本：默认预算约 `120 ms`，长单 token 约 `165 ms`，大量压缩候选约 `55 ms`，深层压缩不完整样本约 `2.87 ms`，精确派生溢出约 `27.9 ms`。80 KiB 级极端编码输入仍是已登记的 CPU/GC 压力风险，不作为“零成本”路径宣传。
  - Verify：所有完成定义和性能门槛同时满足。

## 15. 分批提交边界

获得编码和本地 commit 授权后，每一阶段独立提交，不混入其他阶段：

1. `test(prompt-guard): lock v1 protocol and performance baselines`
2. `perf(prompt-guard): cache request security context`
3. `perf(prompt-guard): reuse newapi verification and body digest`
4. `fix(prompt-guard): unify v1 protocol adapters`
5. `perf(prompt-guard): bound current and auxiliary scan budgets`
6. `perf(prompt-guard): decouple audit writes`
7. `perf(prompt-guard): order asynchronous session writes`
8. `fix(prompt-guard): preserve raw advanced configuration`
9. `feat(admin): expose bounded guard settings visually`
10. `test(prompt-guard): add concurrency memory and ui regressions`

提交前必须运行 GitNexus `detect_changes`。不得使用 `git add -A`，只暂存本阶段明确文件。任何阶段失败时回退该阶段提交，不回退已经验证通过的前一阶段。未获得授权前只保留工作区修改和测试结果，不创建本地 commit。

## 16. 灰度与回滚边界

本轮不部署。未来取得单独授权后：

1. 先部署 Codex2API 蓝绿新实例。
2. 保持处罚、封号和 IP 黑名单关闭。
3. 首先只开启日志与 Shadow。
4. 观察正常请求通过率、首字、队列、摘要次数和日志内容。
5. 再逐步开启当前用户 Prompt 的 Warn/Enforce。
6. 任一指标异常时切回旧颜色，不在生产现场临时改规则代码。

## 17. 完成定义

只有同时满足以下条件才算完成：

- 所有阶段代码和测试完成，工作区没有意外文件。
- GitNexus 影响范围与计划一致，没有未知 V1 流程。
- 全量 Go 测试、Race、Vet、前端测试和基准通过。
- 本地脱敏生产日志语料回放不再因工具输出、系统上下文或路径文本误拦截。
- 明确违规的当前用户 Prompt 在所有受支持 V1 协议得到一致结果。
- 管理员能在 5 分钟内完成安全启用闭环，诊断信息能在 2 分钟内指向字段或文档。
- RawConfig 未知字段、混合版本读取和完整回滚合同通过离线验证。
- README 与五份 Prompt Guard/NewAPI/配置/排障/部署文档已同步，不再包含危险首次启用建议。
- 未创建或修改任何 V2 内容。
- 未部署生产、未启用任何自动处罚、未创建 PR。

## 18. 决策审计

| 决策 | 结论 | 原因 |
|---|---|---|
| 协议版本 | 仅 V1 | 用户明确要求，不允许 V2 |
| 审核对象 | 用户可控且模型可见的输入 | 降低应用上下文和工具输出误杀 |
| 性能修复开关 | 不提供 | 属于内部正确性，不应允许退回重复复制/散列 |
| 辅助上下文 | 默认 Shadow | 保留观测能力，不影响普通请求 |
| 审计与 Session 写入 | 有界异步 | 避免存储延迟影响首字 |
| Realtime 审核时点 | `response.create` 逻辑回合 | 避免把累积/状态帧误当完整 Prompt，并保持真实上游语义 |
| multipart 审核边界 | 允许有界暂存，禁止审核前处理文件 | part 顺序不可靠；既要保留请求又要保证阻断后零图片处理/零上游 |
| 规范化预算 | 来源与输出双重有界 | 防止在截断前发生无界 Unicode/编码规范化成本 |
| 自动处罚 | 关闭 | 先验证审核稳定性，再单独授权灰度 |

## GSTACK REVIEW REPORT

| Review | 关注面 | 轮次 | 结论 | 已固化结果 |
|---|---|---:|---|---|
| `/plan-ceo-review` | 范围与战略 | 1 | CLEAR | V1-only、Codex2API-only、无远端/生产/Git/处罚变更；越界即停止 |
| Codex independent review | 独立工程复核 | 1 | RESOLVED | 规范化前预算、multipart part 顺序、Realtime 逻辑回合三项已并入计划 |
| `/plan-eng-review` | 架构、热路径与测试 | 1 | CLEAR | 显式 Adapter、digest once、有界异步、Raw/Effective 配置、性能门槛与隔离测试完整 |
| `/plan-design-review` | 管理页面与交互 | 1 | CLEAR | 评分 6→9；未知字段、非法 JSON、保存/刷新状态、本地化和无障碍边界明确 |
| `/plan-devex-review` | 首次使用与运维 | 1 | CLEAR | 评分 6→9；安全启用目标 ≤5 分钟，诊断、混合版本写入和完整回滚明确 |

**VERDICT:** CEO + ENG + DESIGN + DX CLEARED — 用户已授权在上述边界内进行本地实施与离线验证；仍未授权 commit、push、PR、部署、生产访问或 Session CAS 接口扩展。

NO UNRESOLVED DECISIONS
