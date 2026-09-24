import assert from 'node:assert/strict'
import test from 'node:test'
import { parseSettingsBackup, SettingsImportError } from './settingsTransfer.ts'

const target = {
  format: 'codex2api.settings', version: 1, exported_at: '2026-09-24T00:00:00Z',
  settings: { site_name: 'target', global_rpm: 100, codex_initial_session_age_check_disabled: false, codex_initial_session_max_age_seconds: 60, model_mapping: '', github_token: '', continuous_retry_status_codes: [500] },
  sections: { invite_guide: { enabled: true }, visible_channels: { channels: ['codex'] }, antigravity: { model_redirects: {}, redirect_overrides_effort: true } },
}

test('portable settings preserve explicit false, zero, empty strings and arrays', () => {
  const source = { ...target, settings: { global_rpm: 0, codex_initial_session_age_check_disabled: true, model_mapping: '', github_token: '', continuous_retry_status_codes: [] }, sections: { invite_guide: { enabled: false }, antigravity: { model_redirects: { 'gemini-3.8-flash': 'gemini-3.8-flash-high' }, redirect_overrides_effort: false } } }
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
  const source = { format: target.format, version: target.version, exported_at: target.exported_at, settings: { codex_initial_session_age_check_disabled: true } }
  assert.deepEqual(parseSettingsBackup(JSON.stringify(source), target), source)
})
