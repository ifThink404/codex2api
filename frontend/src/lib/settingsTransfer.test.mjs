import assert from 'node:assert/strict'
import test from 'node:test'
import { readFileSync } from 'node:fs'
import { parseSettingsBackup, SettingsImportError } from './settingsTransfer.ts'

const target = {
  format: 'codex2api.settings', version: 1, exported_at: '2026-09-24T00:00:00Z',
  settings: { site_name: 'target', global_rpm: 100, codex_prompt_cache_enabled: false, model_mapping: '', github_token: '', continuous_retry_status_codes: [500] },
  sections: { invite_guide: { enabled: true }, visible_channels: { channels: ['codex'] }, antigravity: { model_redirects: {}, redirect_overrides_effort: true } },
}

test('portable settings preserve explicit false, zero, empty strings and arrays', () => {
  const source = { ...target, settings: { global_rpm: 0, model_mapping: '', github_token: '', continuous_retry_status_codes: [] }, sections: { invite_guide: { enabled: false }, antigravity: { model_redirects: { 'gemini-3.8-flash': 'gemini-3.8-flash-high' }, redirect_overrides_effort: false } } }
  const before = structuredClone(target)
  assert.deepEqual(parseSettingsBackup(JSON.stringify(source), target), source)
  assert.deepEqual(target, before)
  assert.equal(Object.hasOwn(parseSettingsBackup(JSON.stringify(source), target).settings, 'site_name'), false)
})

test('foreign files, unsupported versions, protected fields and wrong types are rejected before writes', () => {
  for (const text of [
    'bad json', '[]', '{}', JSON.stringify({ ...target, format: 'accounts' }), JSON.stringify({ ...target, version: 2 }),
    JSON.stringify({ ...target, settings: { admin_secret: 'do-not-change-target-login' } }),
    JSON.stringify({ ...target, settings: { site_name: null } }),
    JSON.stringify({ ...target, settings: { global_rpm: '0' } }),
    JSON.stringify({ ...target, settings: { unsupported_new_setting: true } }),
    JSON.stringify({ ...target, sections: { unknown: {} } }),
    JSON.stringify({ ...target, sections: { invite_guide: { enabled: 'false' } } }),
    JSON.stringify({ ...target, settings: {}, sections: {} }),
    '{"format":"codex2api.settings","version":1,"settings":{"__proto__":{"polluted":true}}}',
  ]) assert.throws(() => parseSettingsBackup(text, target), SettingsImportError, text)
  assert.equal({}.polluted, undefined)
})

test('a backup with only general settings remains importable', () => {
  const source = { format: target.format, version: target.version, exported_at: target.exported_at, settings: { codex_prompt_cache_enabled: true } }
  assert.deepEqual(parseSettingsBackup(JSON.stringify(source), target), source)
})

test('settings transfer card lives in the general tab and uses shared controls, API and locales', () => {
  const settings = readFileSync(new URL('../pages/Settings.tsx', import.meta.url), 'utf8')
  const component = readFileSync(new URL('../components/SettingsTransfer.tsx', import.meta.url), 'utf8')
  const api = readFileSync(new URL('../api.ts', import.meta.url), 'utf8')
  const general = settings.slice(settings.indexOf("activeTab === 'general'"))
  assert.match(general, /<SettingsCard title=\{t\('settings\.transfer\.title'\)\}[^\n]*channels=\{ALL_UPSTREAM_CHANNELS\}>/)
  assert.match(general, /<SettingsTransfer disabled=\{[^}]*dirtyCount > 0\} \/>/)
  assert.match(component, /from '\.\/ui\/button'/)
  assert.match(component, /api\.exportSettings\(\)/)
  assert.doesNotMatch(component, /<select|type="checkbox"/)
  assert.match(api, /exportSettings: \(\) => request<[^\n]*'\/settings\/export'\)/)
  for (const locale of ['zh', 'en', 'zh-TW']) {
    const messages = JSON.parse(readFileSync(new URL(`../locales/${locale}.json`, import.meta.url), 'utf8'))
    for (const key of ['title', 'description', 'export', 'import', 'scope', 'confirm', 'invalid_format', 'invalid_field', 'invalid_type', 'failedSection']) {
      assert.equal(typeof messages.settings.transfer[key], 'string', `${locale} settings.transfer.${key}`)
    }
  }
})
