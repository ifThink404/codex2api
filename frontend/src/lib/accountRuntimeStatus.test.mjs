import assert from 'node:assert/strict'
import { existsSync, readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import test from 'node:test'
import ts from 'typescript'
import React from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import i18next from 'i18next'
import { getAccountStatusBadgeStatus } from './usageFormat.ts'

const src = fileURLToPath(new URL('../', import.meta.url))
const require = createRequire(import.meta.url)
const { I18nextProvider } = require('react-i18next')
const modules = new Map()
// Render the real shared badge and tooltip trigger without a browser or mocks.
function loadComponent(path) {
  const file = [path, `${path}.tsx`, `${path}.ts`].find(existsSync)
  assert.ok(file, `Missing component ${path}`)
  if (modules.has(file)) return modules.get(file)
  const { outputText } = ts.transpileModule(readFileSync(file, 'utf8'), {
    compilerOptions: { module: ts.ModuleKind.CommonJS, jsx: ts.JsxEmit.ReactJSX, esModuleInterop: true },
  })
  const exports = {}
  modules.set(file, exports)
  new Function('require', 'exports', outputText)((name) => {
    if (name.startsWith('@/')) return loadComponent(resolve(src, name.slice(2)))
    if (name.startsWith('.')) return loadComponent(resolve(dirname(file), name))
    return require(name)
  }, exports)
  return exports
}
const StatusBadge = loadComponent(resolve(src, 'components/StatusBadge.tsx')).default
const locales = Object.fromEntries(['zh', 'en', 'zh-TW'].map((locale) => [locale, {
  translation: JSON.parse(readFileSync(resolve(src, `locales/${locale}.json`), 'utf8')),
}]))
const knownReasons = [
  'payment_required', 'payment_required_unknown', 'credential_refresh', 'version_required',
  'usage_limit', 'forbidden', 'quality_degraded', 'grok_empty_stream',
]

async function renderBadge(locale, status, errorMessage) {
  const i18n = i18next.createInstance()
  await i18n.init({ lng: locale, fallbackLng: 'zh', resources: locales, initImmediate: false, showSupportNotice: false })
  return renderToStaticMarkup(React.createElement(I18nextProvider, { i18n },
    React.createElement(StatusBadge, { status, errorMessage })))
}

test('known runtime reasons render localized restricted states instead of unknown', async () => {
  for (const locale of ['zh', 'en', 'zh-TW']) {
    for (const status of knownReasons) {
      const label = locales[locale].translation.status[status]
      assert.ok(label, `${locale}: missing runtime status ${status}`)
      const html = await renderBadge(locale, status)
      assert.ok(html.includes(label), `${locale}: ${status} must render its label`)
      assert.doesNotMatch(html, /data-variant="default"|bg-emerald/)
      assert.equal(getAccountStatusBadgeStatus({ status, openai_responses_api: true }), status)
    }
  }
})

test('payment restriction tooltip includes a qualified reason and the actual upstream error', async () => {
  assert.equal(locales.zh.translation.status.payment_required, '上游拒绝/账单限制')
  for (const locale of ['zh', 'en', 'zh-TW']) {
    const html = await renderBadge(locale, 'payment_required', '  upstream 403: policy denied  ')
    const hint = locales[locale].translation.status.paymentRequiredHint
    assert.ok(hint)
    assert.match(hint, /403/)
    assert.ok(html.includes(hint), `${locale}: generic explanation must remain alongside the error`)
    assert.match(html, /aria-label="[^"]*upstream 403: policy denied/)
    const withoutError = await renderBadge(locale, 'payment_required')
    assert.ok(withoutError.includes(hint), `${locale}: explanation must survive an absent error`)
  }
})

test('real unknown, hard errors and active API status remain distinct', async () => {
  for (const input of [undefined, '', 'unknown', 'future_cooldown_reason']) {
    const status = getAccountStatusBadgeStatus({ status: input, openai_responses_api: true })
    const html = await renderBadge('zh', status)
    assert.match(html, /未知/)
    assert.doesNotMatch(html, /bg-emerald/)
  }
  for (const status of ['error', 'unauthorized', 'paused', 'quota_paused', 'active']) {
    assert.equal(getAccountStatusBadgeStatus({ status, openai_responses_api: true, cooldown_reason: 'payment_required' }), status)
  }
  assert.match(await renderBadge('zh', 'active'), /可用/)
})

test('turn-state samples apply only to official Codex accounts, including legitimate unrecorded samples', async () => {
  assert.ok(existsSync(resolve(src, 'lib/accountTurnState.ts')), 'official turn-state applicability helper is missing')
  const { officialAccountLatestTurnState } = await import('./accountTurnState.ts')
  const unrecorded = { created_at: '2026-09-20T10:00:00Z', turn_state_length: null }
  const received = { ...unrecorded, turn_state_length: 292 }
  for (const latest_turn_state of [unrecorded, received]) {
    assert.equal(officialAccountLatestTurnState({ latest_turn_state }), latest_turn_state)
    for (const provider of ['openai_responses_api', 'grok_api', 'claude_api', 'antigravity_api']) {
      assert.equal(officialAccountLatestTurnState({ [provider]: true, latest_turn_state }), undefined)
    }
  }
  assert.equal(officialAccountLatestTurnState({}), undefined)
  assert.equal(officialAccountLatestTurnState({ latest_turn_state: null }), null)
})

test('account table, cards and details filter turn-state applicability before rendering health', () => {
  for (const [path, count] of [['pages/Accounts.tsx', 2], ['components/AccountDetailSheet.tsx', 1]]) {
    const source = readFileSync(resolve(src, path), 'utf8')
    const healthBars = source.match(/<AccountHealthBar\b[\s\S]*?\/>/g) ?? []
    assert.equal(healthBars.length, count)
    for (const bar of healthBars) {
      assert.match(bar, /latestTurnState=\{officialAccountLatestTurnState\(account\)\}/)
    }
  }
})
