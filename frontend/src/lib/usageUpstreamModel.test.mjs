import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const responseModelSource = readFileSync(new URL('../components/UsageResponseModel.tsx', import.meta.url), 'utf8')
const windowBadgeSource = readFileSync(new URL('../components/UsageWindowNumberBadge.tsx', import.meta.url), 'utf8')
const usageSource = readFileSync(new URL('../pages/Usage.tsx', import.meta.url), 'utf8')
const typesSource = readFileSync(new URL('../types.ts', import.meta.url), 'utf8')
const locales = Object.fromEntries(
  ['zh', 'en', 'zh-TW'].map((name) => [name, JSON.parse(readFileSync(new URL(`../locales/${name}.json`, import.meta.url), 'utf8'))]),
)

const I18N_KEYS = ['responseModel', 'responseModelHint', 'windowNumber', 'windowNumberHint']

test('UsageLog carries the upstream-reported model and the client window number', () => {
  for (const field of ['upstream_response_model?:', 'window_number?:']) {
    assert.ok(typesSource.includes(field), `types.ts UsageLog lacks ${field}`)
  }
})

test('the upstream-model line renders nothing when the upstream declared no model', () => {
  assert.ok(responseModelSource.includes('log.upstream_response_model'), 'the line must read the upstream-reported model')
  assert.match(responseModelSource, /if\s*\(![\w.]+\)\s*return null/, 'an empty value must render nothing at all')
  assert.ok(responseModelSource.includes("t('usage.responseModel')"), 'the label must come from i18n')
  assert.ok(responseModelSource.includes("t('usage.responseModelHint')"), 'the hint must come from i18n')
  // DESIGN.md:40 要求断言「用了哪个共享组件」：只断言 i18n key 的话，把这行换成
  // 硬编码文案或自定义控件仍然全绿。这一行是纯文本，约束落在版式类名上。
  assert.ok(responseModelSource.includes('basis-full min-w-0 break-all text-[11px] text-muted-foreground'), 'the line must keep the muted full-width layout')
})

test('the upstream model is highlighted only when it differs from the requested model', () => {
  assert.ok(responseModelSource.includes('log.effective_model || log.model'), 'the comparison must use the effective model first')
  assert.ok(/toLowerCase\(\)/.test(responseModelSource), 'the comparison must be case-insensitive')
  assert.ok(responseModelSource.includes('text-amber-700 dark:text-amber-300'), 'a differing model must be amber in both themes')
  // 请求侧没有模型名时标琥珀等于报一个不存在的「被换模型」告警。
  assert.match(responseModelSource, /!!requestedModel\s*&&/, 'amber must not fire when there is no requested model to compare against')
})

test('the window-number badge uses the shared Badge and Tooltip', () => {
  assert.ok(windowBadgeSource.includes("from '@/components/ui/badge'"), 'the badge must be components/ui/badge')
  assert.ok(windowBadgeSource.includes("from '@/components/ui/tooltip'"), 'the tooltip must be components/ui/tooltip')
  assert.match(windowBadgeSource, /\/\^\\d\+\$\//, 'a non-numeric window number must render nothing')
  assert.ok(windowBadgeSource.includes("t('usage.windowNumberHint'"), 'the tooltip copy must come from i18n')
  assert.ok(windowBadgeSource.includes('emerald'), 'the badge keeps the emerald outline of the reference implementation')
})

test('neither component hand-writes a form control', () => {
  for (const [name, source] of [['UsageResponseModel.tsx', responseModelSource], ['UsageWindowNumberBadge.tsx', windowBadgeSource]]) {
    assert.equal(/<select[\s>]/.test(source), false, `no hand-written <select> in ${name}`)
    assert.equal(/<input\s+type="checkbox"/.test(source), false, `no raw checkbox in ${name}`)
    assert.equal(/<button[\s>]/.test(source), false, `no raw <button> in ${name}`)
  }
})

test('both usage table cells render the badge and the upstream-model line', () => {
  assert.ok(usageSource.includes("from '../components/UsageResponseModel'"), 'Usage.tsx must import UsageResponseModel')
  assert.ok(usageSource.includes("from '../components/UsageWindowNumberBadge'"), 'Usage.tsx must import UsageWindowNumberBadge')
  // 用量页有两处模型单元格（紧凑卡片与宽表格），只改一处会让另一处永远看不到这两个值。
  const responseModelUses = usageSource.match(/<UsageResponseModel\b/g) ?? []
  const windowBadgeUses = usageSource.match(/<UsageWindowNumberBadge\b/g) ?? []
  assert.equal(responseModelUses.length, 2, 'both model cells must render the upstream-model line')
  assert.equal(windowBadgeUses.length, 2, 'both model cells must render the window-number badge')
  // 窗口号紧跟推理档位徽标，两者是同一行的请求属性。
  assert.match(usageSource, /<ReasoningEffortBadge[^>]*\/>\s*\)\s*:\s*null\}\s*<UsageWindowNumberBadge/, 'the window badge must sit right after the reasoning-effort badge')
})

test('upstream-model and window-number copy exists in every locale', () => {
  for (const [name, locale] of Object.entries(locales)) {
    for (const key of I18N_KEYS) {
      assert.equal(typeof locale.usage?.[key], 'string', `${name}.json usage.${key} missing`)
    }
    assert.ok(locale.usage.windowNumberHint.includes('{{n}}'), `${name}.json usage.windowNumberHint must interpolate {{n}}`)
  }
})
