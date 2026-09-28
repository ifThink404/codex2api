# 账号级 BPS 兼容模式

普通 Codex OAuth / AT 账号的编辑窗口提供“BPS 兼容模式”。新建账号默认关闭 Codex 原生路径，开启 BPS、历史附件精简和跳过预热层级，并开启额度耗尽后的 `gpt-5.6-sol` 放行。跳过预热层级与账号在同一事务中保存，导入后立即生效，重启重载保留；它仅将 warm 调度层级提升为 healthy，不跳过登录刷新、额度检查或封禁状态。已有账号和凭证刷新保留原配置，导入文件中的显式配置优先；没有 `codex_native_enabled` 时统一默认为 false，不从 BPS 开关反推开启 Codex。编辑后点击保存生效。配置键为 credentials.codex_bps_enabled，支持运行时同步、重新加载及原生账号 JSON 导入导出。API 中转和 Agent Identity 不支持开启。

## 会话与请求路径

- 新绑定的主会话把上游模式写入原有 session continuity 记录。既有记录未注明模式时按 native 处理。保存开关不会迁移已有会话；后台请求沿原有父会话绑定继承模式。
- 切号仍使用既有找号、容量和上下文规则，并排除另一种模式的候选账号。
- BPS 使用账号自己的 Access Token、官方 Account ID 和代理，HTTP/SSE 生成地址为 `https://bps.openai.com/basispoints/api/responses`；压缩地址追加 `/compact`。即使全局强制 WS，BPS 仍走 HTTP。不会在失败时自动回落到原生 Codex 路径。
- 单独测连和批量测连均先选择“按账号配置 / Codex / BPS”，再开始测试。选择仅作用于本次测试，不写账号配置，不改变业务会话绑定。显式选择不支持的路径会报错；不会自动换路。BPS 批量测连跳过原生 WHAM 预检，直接验证所选路径。
- 现有账号隐私处理先执行，再做 BPS 投影。Word 网页形态不发送 BPS 设备头；其他形态保留账号设备 ID。普通模式下任务、轮次和内部缓存按调用方、账号隔离；“全部收敛”只将 BPS task 改为上游账号级共用，轮次和内部缓存继续分别管理。原生 Turn-State、客户端自定义握手头不转发到 BPS。

## BPS 全部收敛

账号的设备指纹档位选择 `full`（全部收敛）后，仅 BPS 路径采用以下规则；不修改默认档位、原生 Codex、API 中转或其会话身份策略。

- 同一实际 `Chatgpt-Account-Id` 固定复用一个持久化 UUIDv7 `task_id`，不随下游用户、对话、模型、窗口、换号代次或 BPS 产品类型变化。重复导入相同上游账号也复用；Word、Excel、Sheets、PowerPoint 共用该账号的 task。缺少上游账号 ID 时明确报错，不把所有未知账号合成一个 task。
- `turn_id` 继续按调用方、原对话、轮次、BPS 类型、上游账号和换号代次分区。不同用户即使提供相同原始轮次 ID，也获得不同出站 turn。同轮工具续接保持 turn，`agent_iteration` 从字符串 `"1"` 开始递增，重试复用对应计数。未提供轮次 ID 时沿用用户消息边界推导，缺失显式会话时使用已有隔离缓存分区。
- 切到其他上游账号使用该账号的固定 task，并建立新 turn；切回原账号复用原 task，但换号代次产生新 turn，计数重新开始。SQLite/PostgreSQL 使用既有 UUID 映射和迭代存储，多实例及重启继续复用，不新增存储表。
- 生成和 `/compact` 使用相同映射。历史 input、工具调用及结果、模型、计费、本地窗口、附件缓存和响应缓存不按共享 task 合并。账号选择使用用户隔离的任务种子做软粘性，不按上游共享 task 合并用户；详见 `bps-inferred-conversation-identity.md`。非 Word 类型原有的 Session-Id / prompt_cache_key 仍按原规则发送。
- 诊断 `upstream.bps_compat.full_convergence` 展示实际 task、turn、迭代、持久化状态及 `task_scope=upstream_account`；Word 同时保留 `word_identity`。这是网关提供的账号级 task 映射，不新增或声称 BPS 已支持 parent_task_id / 子线程协议。

关闭 `full` 后恢复相应档位的任务映射。本文其他段落中按对话分配 task 的规则适用于 `off`、`device`、`session` 档位。

## BPS task_id 轮次（第五档）

账号设备指纹档位新增 `round`（task_id 轮次，保留原配置值），可在账号编辑、快速设置、批量编辑及新账号默认档位中选择。现有 `full` 行为不变，已有账号不会自动切换新档位。

系统设置的 **BPS task_id 轮次：每批调用次数** 对应 `bps_round_convergence_limit`，默认 100，允许 1–1000000。只影响选中 `round` 的 BPS 请求；原生 Codex 在该档位仅收敛设备，不把多个原生会话合并。

- 同一实际上游账号按最终模型＋思考等级分别维护批次；同一账号、同一模型、同一有效思考等级跨用户、会话和 BPS 产品共用当前批次的 `task_id`。以 100 为例，每个组合自己的前 100 次新调用使用同一 task，`agent_iteration` 依次为字符串 `"1"` 至 `"100"`；该组合第 101 次调用换 task 并从 `"1"` 开始，不影响其他组合。切回某个组合时继续它自己的批次；映射到相同出站模型的别名共用批次，例如 `codex-auto-review` 与 `gpt-5.6-luna`。
- 思考等级按 BPS 实际映射分组：未提供默认 `low`，`max` 归入 `xhigh`；历史顶层协议 `configuration_update.reasoning.effort` 存在时，以最后一个非空更新为准，不读取消息或工具结果内的同名业务字段。两个模型分别使用 `low / medium / high / xhigh` 时，是 8 组当前 task，按需创建，未使用的组合不预建。压缩调用按其携带的等级分组，未携带仍归入默认 `low`，不会因此向压缩接口增加思考字段。
- 每批 task 在未切号时共用固定 `turn_id`，仅 `agent_iteration` 随新模型调用递增；换批时 task 和 turn 同时更换。切号（包括切回原账号）使用目标账号当前批次的 task，但建立新的 turn；之后在同一迁移段内保持 turn，批次计数继续递增。工具续接及压缩调用也计入。并行工具结果的一批续接计一轮，不按单个工具结果计数。同一账号迁移段内，同一逻辑调用的重试复用原 task、turn、序号，不重复扣轮数；轮换后的旧请求重试也保留旧批次。
- `bps_round_task_lifetime_hours` 控制每批 task 的固定有效期，默认 24 小时，允许 1–8760。每批从首次上游推理发送起计时，持续请求、工具续接及重试不延长截止时间；图片和文件准备、只分配未发送都不启动计时。调用次数达到上限或时间到期后，下一次新调用更换 task_id、turn_id，并从 1 开始，没有新请求就不自动创建。有效期、首次和最后发送时间按批次持久化，重启保留；同账号重试继续使用原批次和编号。修改次数或时长从下一批生效；此配置与 turn_id 轮次的 `bps_turn_task_lifetime_hours` 独立。
- 计数、分配结果及 UUIDv7 均按上游账号、模型和思考等级持久化；重启、重复导入同一个上游账号不会重置。换到其他上游账号时使用该账号对应组合的批次；切回则继续原账号对应组合的计数。
- 修改轮数从下一批生效，当前批次按创建时的上限走完。并发请求按数据库原子分配的顺序编号，不串行等待模型完成，因此上游实际到达或完成顺序不保证与编号一致。
- 编号在准备请求时分配，后续上传或发送失败不会回收；同一逻辑请求重试仍复用该编号。
- 请求诊断 `upstream.bps_compat.round_convergence` 展示 task、turn、序号、`task_model`、`task_reasoning_effort`、`task_generation`、`round_limit`、`task_lifetime_hours`、`reused_step` 和 `task_scope=upstream_account_model_effort_rounds`。发送时还记录 `task_started_at_unix_ms` 和 `task_expires_at_unix_ms`。Word 同时保留 `word_identity`。缺少持久化身份存储时明确失败，不静默退回每次序号为 1。

从旧版未区分模型或思考等级的轮次收敛升级时，首次按新规则请求会为各组合新建批次并从 1 开始；不继续使用旧的混合批次。启动时自动新增 `bps_round_batches` 表记录批次首次及最后发送时间，兼容 SQLite/PostgreSQL，既有计数表和记录保留。升级时为 `bps_round_tasks` / `bps_round_steps` 增补有效期列，旧批次按默认 24 小时固定有效期处理，已有任务、编号和重试映射不改写；截止时间按已记录的首次发送时间计算。到期表示不再为新调用复用旧批次，不物理删除历史映射或日志；同一新版分配的重试、轮换及重启继续遵循上述持久化规则。

新用量日志中，BPS 请求在思考等级旁显示实际出站的 `metadata.agent_iteration`，悬停可看到字段名。该值独立保存为 `bps_agent_iteration`，不依赖用户窗口序号；重试显示原编号，新批次从 1 开始。非 BPS 请求继续显示窗口序号。历史日志不批量回填。

Word BPS 出站只以 `metadata.task_id / turn_id / agent_iteration` 表达任务和轮次，不发送 `Session-Id`、`prompt_cache_key` 或原生 `client_metadata.session_id / thread_id`。因此在 task_id 轮次下，8 组 task 对应 8 组 BPS 任务身份；未切号时每批 turn 固定，切号后按迁移段隔离。入站客户端会话、线程、账号软粘性和本地上下文隔离仍按原规则管理；其他 BPS 产品的 Session-Id / prompt_cache_key 也保留既有隔离规则，不用共享 task 覆盖。

轮次收敛不合并提示词、附件、响应缓存或用户归属，也不替代账号软粘性。批次轮换本身不会触发换号；无显式会话的 BPS 请求继续使用调用方隔离的任务提示参与软粘性选号。

### 从旧版 task_id 轮次升级

修正前的 `round` 每个调用分配独立 turn。升级后使用新批次命名空间，第一次调用建立新的 task 和固定 turn，序号从 1 开始，避免旧批次中途更换 turn 却沿用较大的序号。旧数据库映射保留；升级前请求若再次提交，将按新策略重新分配。之后同一逻辑请求的重试稳定复用编号。

## BPS turn_id 轮次（第六档）

账号选择 `turn_round`（turn_id 轮次）后采用独立的策略，不自动迁移现有账号，也不使用 task_id 轮次的调用次数上限。

- task 按实际上游账号＋最终模型＋有效思考等级分组，与 task_id 轮次使用相同的模型别名、low 默认值和 max→xhigh 规则；不同账号、模型或等级的 task 相互独立。
- `bps_turn_round_limit` 控制每个 turn 批次的**用户提问次数**，默认 100，范围 1–1000000。当前 task 内按已验证用户分别计数，同一用户不同窗口累计；未提供已验证用户时使用调用方 API Key 分区。填 100 时前 100 次新提问共用 turn_id，第 101 次新提问更换 turn。工具续接、压缩和同账号重试不增加提问次数；即使新提问已经换批，旧提问的后续工具结果仍使用其原批次。不同用户不能共享 turn；会话只参与提问去重，不再单独划分 turn 批次。
- `agent_iteration` 与提问次数分开：每次新的模型调用递增，同一逻辑调用的重试复用编号；新出站 turn 从 `"1"` 开始。每个提问使用原始会话和客户端 turn ID 去重；没有客户端 turn 时按最后用户消息边界推导，截断输入且缺少边界时诊断标记 `missing_user_boundary`。并发提问按数据库分配顺序计数，计数及去重结果跨实例和重启保留。上限修改从下一个 turn 批次生效，不改写已分配的提问。
- 系统设置 `bps_turn_task_lifetime_hours` 控制 task 固定有效期，默认 24 小时，范围 1–8760。每个分组从 task **首次上游推理发送**起计时；工具续接、重试及持续使用都不延长截止时间。到期后的第一次新调用建立新 task、turn 并从 1 开始，即使它是上一用户提问的续接。没有新请求不预建 task。只准备或分配但尚未发送不启动计时。
- 同一账号迁移段内，同一逻辑请求的重试保留它原来的 task、turn 和序号，即使 task 已过期或设置已修改。旧 task 重试不影响新 task 的截止时间。更新有效期从下一个 task 生效，当前 task 使用创建时的配置。
- SQLite/PostgreSQL 持久化 task 代次、分配结果及发送时间；进程重启保留状态，多实例通过数据库事务协调轮换。同轮步骤继续使用既有原子迭代计数。切号使用目标账号对应模型＋思考等级的当前 task，并建立新 turn、序号从 1 开始；切回原账号也不能复用上次驻留的 turn。切号本身不刷新目标 task 的有效期。
- 诊断 `upstream.bps_compat.turn_convergence` 展示 task、turn、`agent_iteration`、实际模型和思考等级、代次、`task_lifetime_hours`、首次/最后发送及过期时间；`turn_question_limit`、`turn_question_number` 和 `turn_generation` 分别表示本批提问上限、该提问在批次中的序号和 turn 批次代次。`task_scope=upstream_account_model_effort_timed_turns`，Word 同时记录 `word_identity`，日志序号徽标继续显示实际出站 `agent_iteration`。
- 设置页将 task_id 轮次与 turn_id 轮次分开显示。前者的模型调用次数不影响后者；后者分别填写用户提问次数和 task 有效小时数。升级到按用户提问分批的实现后，首次调用建立新的 turn 批次，task 计时不重置；旧的逐提问 turn 映射保留，之后的重试按新批次分配稳定复用。
- task 收敛不合并本地提示词、附件缓存、响应缓存或用户归属；原生 Codex 仅收敛设备。账号编辑、快速设置、批量编辑和新账号默认档位均可选择新模式。

两种轮次模式的 turn 隔离优先使用持久化会话迁移代次；没有显式会话、使用任务软粘性选号时，使用绑定账号的切换版本。软粘性只在换账号时递增 `binding_revision`，同账号续接和重试保持版本；写入失败或并发竞争未获绑定的请求使用独立的临时 turn 范围，不复用胜出账号的 turn。临时后台请求在每次实际换号时递增自己的迁移代次，不写入主会话归属。

## 格式转换

| 内容 | BPS 行为 |
| --- | --- |
| `codex-auto-review` | BPS 路径映射为 `gpt-5.6-luna`；日志保留 requested_model=codex-auto-review、sent_model=gpt-5.6-luna 并记录映射动作。其他模型及原生 Codex 路径维持原选择 |
| `input`、历史工具调用和结果、加密历史 | 经过既有隐私处理后保留；不删除工具业务内容 |
| 顶层 `instructions` | 放入 developer 输入消息 |
| 顶层 `tools` | 放入 additional_tools 输入项；已有 additional_tools 保留 |
| 通用助手身份 | input 最前面追加固定 developer 兼容消息，抑制 Office 身份与内置工具使用；调用方提示词、工具、历史仍保留 |
| `reasoning.effort` | 生成请求使用顶层 `reasoning_effort` |
| `client_metadata` / 顶层 `metadata` | 转为 BPS task_id、turn_id、工具版本与 agent_iteration；原身份元数据不透传 |
| 生成 | model、input、metadata、model_selection=explicit、stream=true、store=false、reasoning_effort；Word 不发送 prompt_cache_key，其他形态保留 |
| Word context_management | 用户传入值原样保留；未传不补默认阈值 |
| service_tier（含 priority / flex） | 生成与压缩均不发送顶层字段；续接 input 内 configuration_update 的同名控制字段也移除。原入站值保留在请求诊断中，移除位置记入 removed_fields；消息文字、工具参数及工具结果内的业务数据保持原样 |
| 压缩 | 只发送 model、input、metadata；不发送 prompt_cache_key 等生成参数 |

## Word 网页请求对齐（全局）

Word BPS 使用官方 Word 网页加载项实测字段；所有 Word BPS 请求直接生效，无单账号灰度或 legacy 开关。Excel、PowerPoint、Sheets 维持原规则。已在运行的请求保持构造时的快照；更新后的下一次 Word 请求使用新的持久化 UUIDv7 映射。

- `metadata.task_id`、`turn_id` 为独立 UUIDv7。同逻辑会话保持 task，新用户轮次更换 turn，同轮工具续接保持 turn。明确客户端标识优先；无轮次 ID 时通过最近用户消息边界推断并记录来源。
- 内部账号、调用方和换号代次参与分区。更换账号或建立新的换号代次后，新 turn 的字符串 `agent_iteration` 从 `"1"` 开始，不累计上一账号的历史工具次数；`task_id` 轮次模式例外，继续目标账号当前 task 批次的计数。
- 每批工具结果引发一次新的推理时迭代递增；同一批并行工具结果改变顺序、网络重试和附件重试不重复增加。SQLite/PostgreSQL 持久化保证多实例竞争及重启后复用。无数据库的独立投影使用确定性 ID 和历史工具批次数，并标记 persisted=false，不承诺缺失历史的完整续接恢复。
- 同一会话的上下文窗口/缓存提示变化不更换明确会话的 task。无会话 ID 的“设备＋会话”启发式仍有原限制：同设备相同开头无法可靠区分独立对话。
- 使用 Agent-Profile=`word`、Host=`office`、Runtime=`web`、Platform-Class/Office-Platform=`OfficeOnline`。不再生成 `Mac / 16.113`、Version、Originator、Session-Id、Codex Responses Lite、BPS Client-Device-Id 或 Tools-Version 请求头。工具版本仍发送在 metadata 中。
- 设置 → 客户端形态 → **Word BPS** 可编辑 UA，使用原有“保存身份配置”保存；池模式也提供独立 Word UA 输入框。配置键为 `codex_user_agent_config.bps_word_user_agent`。留空默认为实测 Windows Chrome 150 UA，独立于其他客户端形态与其版本同步。
- 用量日志的上游 UA 摘要取 BPS 最终请求头，与 `upstream.outbound_identity.http.headers.User-Agent` 一致。此前摘要误取了准备阶段的 Codex UA；实际发送的 Word UA 已正确，历史日志不会回填。
- 仅移除对外发送的 prompt_cache_key；内部图片/文件缓存与账号隔离仍存在。context_management 依用户原值发送，不自动填入 200000。不改变用户工具 namespace、参数、call_id、推理强度及 service_tier 计费规则。
- `word_identity` 诊断记录明文 task_id、turn_id、agent_iteration、turn_source、persisted、reused_step、generation。出站请求体 metadata 中的这些 ID 也显示原值，不做哈希或 UUID 隐藏；认证凭据仍不记录。

此对齐不构成“旧请求字段导致 403 或慢首字”的根因结论；也不模拟不存在的 Word 子智能体能力。

图片适配仅访问消息 content 和 function/custom 工具结果 output 数组中的 input_image。对内嵌 Base64 图片按真实 PNG/JPEG/GIF/WebP 字节修正 MIME，保留图片 detail、消息顺序及工具 call_id；默认不把图片当文本、不丢弃图片，不改工具参数或字符串中的业务 JSON，也不主动下载远程 URL。显式开启下文的历史附件精简后，符合条件的旧图会被文字占位替代。

用户消息及 function/custom 工具结果中的内嵌图片按上游前端的附件流程处理：使用当前账号和相同代理向 `/attachments` 逐张上传原始图片字节，将 image_url 替换为 file_id。用户消息保留原 content 位置；工具图片引用使用下述附件消息兼容方式。只改变引用和载体，不压缩、转码或伪造工具调用，已有 file_id 保留。需要上传的内嵌图片无效时明确报错，不删除图片后继续请求。普通生成和 compact 共用此适配；开启历史附件精简时先裁剪再上传，不上传已省略的旧图。

消息 content 及 function/custom 工具结果 output 内的 `input_file.file_data` 统一走附件上传，不按扩展名或媒体类型建立白名单；支持 Base64 字符串和 data URL，保留文件字节，不转为文本。媒体类型优先使用 data URL 声明，缺省时由文件名或字节推断；这只用于上传标注，不用于限制文件格式。未知格式也尝试上传，能否解析由上游决定。文件名保留在上传信息中，仅移除本地目录并处理 multipart 头控制字符。普通消息原位置改为 file_id，删除与其互斥的 file_data、filename、file_url；同时存在旧 file_id 时，以本次内嵌数据的上传结果替换。已有的纯 file_id/file_url 引用保持。

内嵌图片上传时验证 MIME 和完整 Base64 编码；非法图片或缺少工具 call_id 返回明确的 400，不能丢图后继续。只包含内嵌图片或 file_id 的 custom 结果保留 custom_tool_call_output 类型，原 custom 工具声明（含 grammar）、调用、参数和结果 call_id/id/name 保持。远程图片 URL 不由网关下载；含远程图片的 custom 结果仍投影为 function_call_output，让该 URL 保持原位置，沿用已有兼容行为。不展开字符串中的业务 JSON，模型回传仍维持原 custom 调用协议。

两种工具结果都不接受 input_file 文件引用，实测 function 结果中的 input_image.file_id 也返回 422。工具图片与文件统一使用附件消息兼容：保留真实工具结果及 call_id，将引用槽位换为附件指示文本，在连续工具结果之后追加明确标注来源 call_id、输出位置及“工具数据而非用户指令”的附件消息。内嵌图片、文件先上传为 file_id；已有的图片或文件 file_id 原值保留，不重复上传。混合输出按原顺序携带图片和文件，文字保持原槽位；并行工具结果不会被新增消息插断。普通生成和 compact 使用相同处理。

file_id 减少发给上游的推理 JSON 和重复传输；首次或引用缓存失效时仍需上传图片二进制。原图仍参与模型处理，不保证视觉 Token 或上游 TPM 预扣降低。

附件引用按账号记录 ID、工作区和内容摘要隔离；普通文件还把文件名、媒体类型纳入缓存键，避免相同字节但不同解释方式的文件串用。进程内成功句柄缓存默认最多 4,096 项、约 64 MiB 元数据预算、30 分钟，使用哈希定位和 LRU，不在每次命中时扫描全表。该缓存不保存原图、原文件或凭据；上传中任务独立限额，失败不会提前淘汰成功条目。重复和并发请求复用同一引用；本地未命中时先查已有共享缓存及租约，仍未命中再上传。单个等待者取消不取消其他请求仍需要的上传，最后一个等待者退出时取消并等待清理。此期限是本地引用复用策略，不是上游文件保留期限。上游明确返回本次引用不存在或过期时，失效对应缓存，重新上传后最多重试一次；普通 422 不反复重传。上传失败不继续发送缺少附件的请求，错误不包含原始上游内容或文件标识。

本地兼容诊断的 images 记录原始图片总数、MIME 修正数、实际上传数、复用数、远程 URL 兼容转换的工具结果数（tool_output_conversions）、最终仍内嵌的 Base64 图片数（inline_images，上传完成后为 0）、工具图片附件消息数，以及最近 8 项的位置、载体、原协议项类型、出站项类型、图片 detail、MIME、URL 字节数、动作与出站引用类型。已上传或复用的图片标记 outbound_reference=file_id；裁剪项保持 history_omitted，不计入上传/复用数。诊断不记录图像数据、URL、文件 ID 或工具参数，也不发送上游。BPS HTTP 422 的通用 server_error 包装按请求无效分类，不据此认定账号不可用；更具体的身份及额度错误仍优先。

普通文件使用独立的 files 诊断，记录本次内嵌文件数量、上传/复用数、工具附件消息数，以及最多最近 8 项的位置、载体、字节数和动作。不记录文件名、文件内容、内容摘要、MIME 参数或真实文件 ID，不发送上游。原生 Codex 路线和 API 中转账号不进入 BPS 附件适配。

images/files 中的位置指追加工具附件消息之前的 BPS input 投影位置，用于对应转换来源；工具附件消息可能使最终出站的后续 input 索引后移。

BPS 是经过实测的兼容投影，服务端不接受的顶层控制字段（包括 service_tier、text、tool_choice、parallel_tool_calls、include 等）不发送。按用户选择，不增加 JSON Schema 校验或并行控制执行层；这些控制不保证等价支持。原始提示和工具定义仍保留。其他未进入上述投影的顶层字段同样记录为移除字段，方便排查。若上游意外要求调用客户端未声明、名称带提供方标记的内部工具，返回处理失败，不伪造工具别名或丢弃调用后继续宣称成功。

固定兼容消息来自已验证的调用格式，只改变回答行为；不会让服务端停止加载内置提示词或减少其实际输入用量。下文的固定扣减是本地计费优惠口径，原始上游用量单独留在诊断中，不代表上游上下文已清空或官方额度消耗减少。

附件缓存、单请求/实例并发、缓冲准入及部署参数详见 [附件性能配置](bps-attachment-performance.md)。

## BPS 输入与缓存的固定扣减

普通 BPS 生成请求按账号本次选中的类型，从上游输入及缓存中扣除固定内置开销；原生 Codex、其他提供方和未验证基线的 `/compact` 请求不使用此扣减。无需额外开关，账号类型切换在下一次请求生效，已开始的请求继续使用其类型快照。

基线来自 2026-09-24 同一授权账号、同一代理配置、`gpt-6-astra` / `low` 的实测：只发送 `input="1"`，不带调用方 instructions/tools，各类型分别使用新缓存标识，并在该标识下原样重复一次。仍发送现有 developer 兼容消息与上游内置提示词/工具。8 次请求均返回 HTTP 200 和 `response.completed`。

| 类型 | 首轮与重复请求的原始输入 | 首轮缓存写入 / 重复请求缓存命中 | 固定扣减量 |
| --- | ---: | ---: | ---: |
| Word | 13,921 | 13,853 | 13,920 |
| Excel | 22,949 | 22,881 | 22,948 |
| Sheets | 18,182 | 18,114 | 18,181 |
| PowerPoint | 37,680 | 37,612 | 37,679 |

扣减量取实测输入减 1，为探针文本本身保留 1 个计费输入 token。这是按类型统一执行的本地固定计费政策，不宣称精确恢复模型的内部提示词 token 数，也不使用任意真实用户首轮去动态校准。其他模型和推理强度没有在本轮验证相同基线；补测 `gpt-6-sol` 返回 403 `basispoints_model_access_changed` 后停止，没有换号或重试。上游提示词、工具版本或本地 developer 兼容消息改变后应重新标定这些常量。

设原始输入为 I、缓存命中为 C、缓存写入为 W、类型扣减量为 H：

- 对外输入与计费输入：`I' = max(0, I - H)`。
- 缓存读取：`C' = min(I', max(0, min(C, I) - H))`；缓存是输入的子集，不从 I' 再扣一次作为输入总量。
- 缓存写入：先扣掉已命中的内置前缀，剩余扣减量 `Hw = max(0, H - min(C, I))`，再计算 `W' = min(I' - C', max(0, W - Hw))`。避免同一固定前缀从读缓存、写缓存重复扣除。
- 输出及推理输出 token 保持上游值，`total_tokens = I' + output_tokens`。用户工具、历史、图片等新增输入仍保留在 I' 中；不会每轮重新估算整份请求。

因此上述 `1` 探针在首轮和重复请求中均按输入 1、缓存读取 0、缓存写入 0 计费；观察到的原始输入与缓存相差 68，不能把 68 当成全部内置开销。

扣减在 HTTP JSON / SSE 响应进入常规用量提取之前完成，codex2api 日志、客户端 usage 和 NewAPI 收到的用量采用同一口径。NewAPI 原有“输入减缓存、缓存单独计价”逻辑无需再次扣减。响应 usage 标明 `billing_source=fixed_runtime_overhead_v1`、`input_tokens_estimated=true`；重复经过响应处理不会再次扣减。缺失或非法用量不制造数据，历史日志和已发生的账单不重算。

本地 `usage_billing` 诊断保留基线来源、固定扣减量、原始 input/cache read/cache write/output 与扣减后的计费值。官方用量窗口、上游限流和实际官方额度仍由上游决定。回归测试覆盖四种类型冷/热缓存、混合缓存读写、较短请求归零、长历史、图片、JSON/SSE、原生及 compact 不受影响、重复投影和实际计费拆分。

## 日志

使用日志端点旁显示 BPS，实际地址记录为 BPS 地址，实际传输标记 HTTP。请求诊断及测连展示请求模型、实际发送模型、上游自报模型（仅上游提供时）、格式转换和移除的字段名。原始模型仍保留在请求诊断中。

本地出站快照记录 BPS 身份头和 metadata 的脱敏值；配置、诊断和转换记录不发送上游。BPS 返回的任务标识沿既有响应隐私边界处理，工具参数等业务内容不按身份元数据整体清理。

BPS 正常响应头未携带 Turn-State 时，向客户端补充本地模拟令牌：217 字节、292 字符、URL-safe Base64，带版本、时间戳、随机数据及本地 HMAC。同一用户/线程/轮次/账号/代次复用，换轮或换号后更换。模拟类型写入认证的持久化绑定，回传时只校验并清除，不恢复成上游状态，不作为找号或保活凭据；原生 Codex 路径不补模拟值。HTTP 头和流式 metadata 使用同一值。

日志保留独立的真实上游观察值与 `client_turn_state`（source=synthetic）。上游实际 0 字符不会被改写成 292；测连也分开显示。模拟令牌不代表缓存命中或模型能力改变。

账号批量编辑用一个“修改 Codex / BPS 调度设置”总开关控制整组设置。总开关默认关闭，不提交该组任何字段；开启后展开 Codex / BPS 开关、额度放行名单、BPS 类型、附件精简、原生压缩限制和设备指纹档位，保存时统一应用到所选账号。每次打开批量编辑时，Codex 默认关闭，BPS、历史附件精简及 `gpt-5.6-sol` 额度放行默认开启；可以手动开启 Codex 后保存，总开关关闭时仍保留所选账号原配置。

## 公开响应与测连预览的来源清理

公开地址统一投影为 `https://chatgpt.com/backend-api/codex/responses`，压缩路径为其 `/compact`。真实连接目标和本地路由诊断保留；支持大小写、URL 编码、JSON Unicode／斜杠转义以及旧占位域名。覆盖响应头、JSON、公开错误和 SSE 文本／工具参数增量；跨事件重组后替换，支持地址长度变化，并保留事件顺序。

BPS 响应中的 instructions、tools、metadata 投影为该请求调用方提供的内容；工具集合包括顶层 tools、input.additional_tools 和 tool_search_output 中的声明。服务端内置提示词、工具定义和 BPS 元数据不进入普通回传或测连预览。作用范围由实际出站请求上下文确定，不根据账号开关的后续变化猜测。原生 Codex 和普通 API 中转维持原处理。

测连复制／下载使用中性的 `compatibility` 字段及显示地址，不再导出 `bps_compat.mode=bps`；单独和批量测连都经过该边界。清理在正文预览截断之前完成。界面将此标为“响应预览”，避免将清理后的内容称为未处理原文。对外采用上文的计费用量，原始上游用量另存 `usage_billing` 诊断；响应 ID、原生 Turn-State 与模拟令牌的分别观测仍保留。本地使用日志中的路由审计、账号模式配置保留，便于运维。

提供方生成的文本还会替换已知的 Basis Points 等来源字样；调用方的工具参数、JSON Schema、签名和加密历史不做这类品牌词替换。它不是任意编码或图片内容的语义识别，也不能保证模型永远不以其他措辞描述上游环境。

NewAPI 管理员和 Root 的已验签请求可自动豁免回答及工具参数中的来源名称和地址替换。该豁免同样适用于原生 Codex 响应；凭据和协议身份过滤继续执行。需要同时更新两端，详见 [下游响应隐私边界](downstream-response-privacy.md) 的管理员来源文字豁免说明。

这是对明确域名及其已覆盖编码形式的过滤，不是对模型语义、任意编码或图片内容的推断过滤。

## 账号 BPS 类型（默认 Word）

账号编辑支持 Word、Excel、Sheets、PowerPoint 四个互斥开关，必须保持一个选中。后台保存单一字段 `codex_bps_profile`，可选值为 `word`、`excel`、`sheets`、`powerpoint`；旧账号缺少该字段时按 Word 处理。更新接口拒绝空值、null、数组和未知类型。仅普通 Codex OAuth / AT 账号可配置；BPS 总开关、Codex 开关和两条路径的模型设置分别保存。

批量编辑先开启“修改 Codex / BPS 调度设置”，再选择要统一应用的类型；总开关关闭时不覆盖各账号的原值。该字段支持持久化、账号列表、导入导出和调度节点同步，不需要新增数据库列。批量包含不支持的账号时整批返回错误，不部分修改。

| 类型 | 工具版本 |
| --- | --- |
| Word | `tools-word-core-2026-08-17-5b142653` |
| Excel | `tools-excel-core-2026-06-16-3af59f22` |
| Sheets | `tools-sheets-core-2026-06-01-34ba0624` |
| PowerPoint | `tools-powerpoint-core-2026-08-10-2b886486` |

一次请求开始时读取类型快照，正文、请求头、压缩和附件请求使用同一套配置。Word 保留原请求头、developer 适配指令和缓存标识规则；其他类型采用对应产品请求头、工具版本及补充的 developer 宿主适配说明。Sheets 不发送 Office-* 宿主头。所有类型仍走 `https://bps.openai.com/basispoints/api`，并不据此假定独立额度或容量池。

保存后下一次 BPS 请求使用新类型，正在进行的请求保持原快照。不同类型的内部缓存和 task_id 分开生成；Word 不发送 Session-Id，其他类型仍有各自的 Session-Id。仍保留调用方历史、加密压缩项和工具输出，不清空用户会话，也不将 BPS 会话迁移到原生 Codex。跨类型切换已有加密上下文的兼容性不能仅靠缓存隔离保证。

developer 适配只引导模型避免依赖客户端没有提供的 Office/Sheets 环境。调用方声明的工具仍通过 additional_tools 提交，响应投影保留其 schema、call_id、参数及结果；此功能没有删除上游内置提示词/工具，也不减少它们的输入 token。提示词引导不能保证永远不会误调用内置工具或透露宿主信息。模型自述必须与上游结构化响应分开解释。

本地 BPS 诊断增加 `profile` 和 `tools_version`，用于核对实际出站类型。回归测试覆盖四种类型的普通/压缩请求、默认兼容、缓存隔离、用户工具/压缩历史保留、配置保存与批量编辑。

2026-09-24 同账号、同代理配置、`gpt-6-astra` / `low` 的实测中，四种类型都完成简短文本、用户工具调用及结果续接、HTML/SVG 生成。两类信息探测共 8 次没有获得可核实的内部身份或模型 checkpoint；成功响应的 `response.model` 均报告 `gpt-6-astra`，不据此独立鉴定模型权重。首轮实验的 namespace 缺少必填 description 导致 400，补齐实验 schema 后工具调用和续接全部通过。其他类型的实时附件及真实压缩未在本轮测试。

## 历史附件精简（新账号默认开启）

账号编辑和批量编辑中的开关统一命名为 **BPS 历史附件精简**，图片与普通文件共同受控。保存字段仍是 `codex_bps_image_trim_enabled`，兼容原配置、重启加载、批量编辑和导入导出，不新增第二个开关。已有 true/false 值保持，升级后原来开启该开关的账号同时对历史文件生效。

用于 BPS 用户主请求、普通 fork 和明确标记为 `thread_spawn` / `collab_spawn` 的普通子任务轮次；有根会话关联不再被当成跳过精简的理由。开启后保留当前用户轮的图片和文件、**图片与文件合计最近 3 个工具附件**、最新一整组附件结果，以及历史中尚无后续模型内容的附件。不是图片 3 个再加文件 3 个。最新多附件结果和未处理完的并行结果可能超过 3 个，避免丢失本轮刚取得的数据。

请求或单项缺少轮次标记时，改用已提供历史的顺序：保留最新用户消息片段组；仅在有中间模型消息证明其属于更早用户段时精简未标记的旧用户附件。已被后续模型内容消费的旧工具附件参与最近 3 个的选择。连续用户片段、未知附件结构仍保留。压缩、审核、分类、记忆、身份冲突及未识别的后台请求不裁剪；不会为精简伪造 turn、task 或会话标识。

其余旧 `input_image` / `input_file` 在上传前替换为来源引用和省略提示，不再上传被省略的内嵌内容。只保留原附件已有的文件名、URL、file_id，以及原消息中的路径标签和工具调用；不把 file_data / data URL 复制到提示里，不为了生成地址再上传旧文件，也不凭空生成下载地址。只有文件名或 file_id 不代表能重新获取原内容；没有可访问路径或有效来源时可能无法恢复，提示会明确说明这一点。

原调用、call_id、相邻文字、音频、视频、工具参数和字符串中的业务 JSON 保持。来源信息在省略提示中标为引用数据；新生成的用户内容槽不会标记成用户指令。客户端完整历史仍保留，处理只作用于上游发送副本。需要恢复时，可由客户端已有工具重新读取原始来源；本功能不增加服务端附件存档、下载服务或自动摘要，也不读取用户电脑的会话文件。

裁剪由输入历史确定，不按请求次数或文件 hash 累计；相同输入重试得到同一结果，同一文件在新的工具结果中再次出现时受到新结果保护。省略属于有损上下文整理，会影响模型可见内容和缓存前缀，不保证按文件字节比例降低 Token 或 TPM。关闭原开关后图片、文件均不再按这套历史规则精简。

为兼容现有诊断读取器，沿用 `image_history` 外层字段与 `images_before/after/omitted`；策略为 `recent_attachments_v2`，保留 `recent_tool_attachments`、`attachments_before/after/omitted`、`files_before/after/omitted`。`boundary_source` 说明使用轮次信息加历史顺序或仅历史顺序；`retained_reasons` 按互斥保留原因计数（当前用户轮、最新用户段、最近工具附件、最新整组、未消费结果、未知结构、整项策略跳过）。跳过请求也记录真实附件数量。引用字节和输入项字节涵盖两种附件，最多记录 8 个省略位置，不记录文件名、Base64、完整路径、URL、文件 ID 或客户端身份，也不发送上游。已有 `images.count` 表示原始图片数；上传阶段的 `files.count` 只统计精简后仍需处理的内嵌文件。
