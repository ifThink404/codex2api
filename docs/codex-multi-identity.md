# 多身份模式与设备 ID

身份设置包含单一画像、号池分布和多身份三种模式。多身份模式启用 CLI、桌面端、VS Code、exec 四份配置，以入站 User-Agent 的客户端名前缀选择；缺失或无法匹配时使用自定义配置。版本、操作系统、终端和设备 ID 不取自用户输入。账号自定义 User-Agent / Version 和既有设备配置的优先级保持不变，最终 Originator 从实际发出的 UA 生成。

| 入站客户端名（斜线前，忽略大小写） | 使用配置 |
| --- | --- |
| codex-tui、codex_cli_rs | Codex CLI |
| Codex Desktop、codex_app、codex_chatgpt_desktop、codex_work_desktop、codex_atlas | 桌面端 |
| codex_vscode | VS Code |
| codex_exec | exec |
| 其他或缺失 | 自定义 |

客户端形态按钮选择的是正在编辑的配置，不是多身份模式的唯一生效身份。切换客户端、身份模式、平台或终端仅修改草稿，输入框失焦不保存；“保存身份配置”或页面保存才持久化并应用。切换回来恢复对应草稿，保存后重新加载仍保留各客户端配置；保存过程中继续输入时，不会被较早的保存响应覆盖。未保存草稿不会跨页面刷新持久化。

配置沿用 codex_user_agent_config JSON，在 profiles 下保存五种客户端各自的字段。原有扁平字段继续表示单一模式的当前配置，旧配置切换时归入原客户端。号池分布继续使用既有目录和配比，不改变原有算法。

## 官方设备 ID 依据

核查 2026-09-20 本地官方引擎 0.155.0-alpha.9，对应源码提交 434535bddfaf405a032f57be3c1096dd25ff6312：

- [installation_id.rs](https://github.com/openai/codex/blob/434535bddfaf405a032f57be3c1096dd25ff6312/codex-rs/core/src/installation_id.rs)：在 CODEX_HOME/installation_id 上加文件锁；存在合法 UUID 就复用，否则生成 UUIDv4 并写回。没有按客户端或账号派生。
- [home-dir](https://github.com/openai/codex/blob/434535bddfaf405a032f57be3c1096dd25ff6312/codex-rs/utils/home-dir/src/lib.rs)：CODEX_HOME 可覆盖目录，默认用户家目录下的 .codex。
- [官方配置文档](https://learn.chatgpt.com/docs/config-file/config-basic)：CLI 和 IDE 扩展共享配置层。
- 本机桌面 26.915.31029 的 worker.js 也按 CODEX_HOME / homedir/.codex 解析；WSL 模式可解析 WSL 环境目录。桌面不是必然使用另一套 ID。

因此同一用户、同一个 CODEX_HOME 下的不同客户端共用安装 ID 是正常行为；不同机器、用户、独立 CODEX_HOME 或远程运行环境可能不同。“同一个账号”在官方实现里并不等于“所有设备同一个 ID”。

网关继续使用账号已有稳定安装 ID，切换客户端不重新生成，不使用入站用户 ID。它表示网关配置的固定安装身份，不是声称官方按账号生成 ID。HTTP 头、正文元数据和 WS 握手保持同值；现有账号自定义设备覆盖仍生效。各客户端平台字段可独立配置；若这些客户端用来表示同一台设备，应配置相容的平台信息。

## 验证

- 草稿测试覆盖往返切换、模式切换、保存重载、未完成输入和保存期间的新修改。
- 选择器覆盖四类客户端、别名、未知前缀及缺失 UA；预览不修改运行配置。
- 本地模拟上游捕获 18 次 HTTP / compact / WS 请求，验证最终 UA、Version、Originator 和设备字段一致；更换客户端不复用冲突的 WS 握手，同一身份仍复用连接。
- 辅助端点、API 中转和 Live 请求头测试验证同样的选择与账号覆盖优先级。
- 上述测试不消耗真实上游额度，也不代表已在生产服务器部署。
