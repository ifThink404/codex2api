# DESIGN.md — 前端 UI 约束

本文件是 `frontend/` 的组件与布局约束。所有前端改动（含 AI 代理生成的代码）必须遵守；
`frontend/src/lib/uiConventions.test.mjs` 与 `claudeParity.test.mjs` 会在 CI 中强制其中可机检的部分。

## 1. 表单控件：只用共享组件，不手写

| 需求 | 必须使用 | 禁止 |
|---|---|---|
| 下拉选择 | `components/ui/select.tsx` 的 `Select`（`options` 数组，`value` / `onValueChange`） | 原生 `<select>` / 自定义 className 字符串（如 `selectCls`） |
| 开关 | `components/ui/switch` 的 `Switch` | `<input type="checkbox">` |
| 数字输入 | `components/ui/draft-number-input` 的 `DraftNumberInput`（带 `min` / `max`） | `<Input type="number">`（仅历史遗留允许） |
| 文本输入 | `components/ui/input` 的 `Input` | 原生 `<input>` |
| 少量互斥选项 | `Settings.tsx` 的 `SegmentedPillGroup` | 手写按钮组 |
| 按钮 | `components/ui/button` 的 `Button`，图标用 lucide，加载态用 `RefreshCw` + `animate-spin` | 原生 `<button>` |

需要新的表单控件时，先在 `components/ui/` 新增共享组件，再在页面使用；不在页面内部就地实现。

## 2. 设置页布局

- 每个配置模块用 `SettingsCard`（`title` / `description` / `icon` / `footer`）。
- 单个配置项用 `SettingField`（`label` / `description` / `layout="switch"` 可选），说明性提示用 `SettingHelp`。
- 栅格只用 `SETTINGS_FIELD_GRID` / `SETTINGS_FIELD_GRID_3` / `SETTINGS_SWITCH_GRID` 常量，不手写 `grid-cols-*`。
- "开关 + 数值"成对的行（例如自动同步 + 间隔）沿用 Codex 运行时优化区块的两列边框布局；新增同类区块直接复制该结构。
- 版本号、ID 等等宽内容用 `font-mono text-xs text-muted-foreground`。

## 3. 文案

- 所有可见文案走 `t('namespace.key')`；新增 key 必须同时写入 `locales/zh.json`、`en.json`、`zh-TW.json` 三个文件的同一位置。
- 占位符用 `{{name}}`，不用字符串拼接。

## 4. 守卫测试

- 新增或改动设置区块时，在 `frontend/src/lib/claudeParity.test.mjs`（Claude 相关）或对应的源码守卫测试里加断言，覆盖：使用了哪个共享组件、调用了哪个 API 方法、i18n key 存在。
- `uiConventions.test.mjs` 会扫描 `pages/` 与 `components/`（排除 `components/ui/select.tsx`）拒绝任何原生 `<select>`。

## 5. 参照实现

- 共享下拉：`frontend/src/pages/Settings.tsx` ClaudeCode 卡片的时区 / 指纹模式 / 平台 / 版本策略字段。
- 同步按钮 + 自动同步开关 + 间隔：Settings.tsx 中 Codex "运行时优化" 与 ClaudeCode "CLI 版本同步" 区块。
