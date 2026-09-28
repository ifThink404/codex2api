import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const settingsPage = readFileSync(new URL('../pages/Settings.tsx', import.meta.url), 'utf8')
const types = readFileSync(new URL('../types.ts', import.meta.url), 'utf8')
const locales = ['zh', 'en', 'zh-TW'].map((name) => JSON.parse(readFileSync(new URL(`../locales/${name}.json`, import.meta.url), 'utf8')))

test('web search proxy location is an auto-saved Switch in the Codex settings', () => {
  assert.match(types, /codex_web_search_proxy_location: boolean/)
  assert.match(settingsPage, /<Switch\s+checked=\{settingsForm\.codex_web_search_proxy_location\}\s+onCheckedChange=\{\(checked\) => autoSaveBooleanField\('codex_web_search_proxy_location', checked\)\}/)
  assert.match(settingsPage, /codex_web_search_proxy_location: cacheNormalized\.codex_web_search_proxy_location \?\? false/)
})

test('web search proxy location strings exist in every locale', () => {
  for (const locale of locales) {
    assert.ok(locale.settings.codexWebSearchProxyLocation)
    assert.ok(locale.settings.codexWebSearchProxyLocationDesc)
  }
})
