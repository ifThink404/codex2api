import assert from 'node:assert/strict'
import test from 'node:test'
import { readFileSync } from 'node:fs'
import { parseSettingsBackup, SETTINGS_SECRET_FIELDS, SettingsImportError } from './settingsTransfer.ts'

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
  assert.match(component, /api\.exportSettings\(includeSecrets\)/)
  assert.doesNotMatch(component, /<select|type="checkbox"/)
  assert.match(api, /exportSettings: \(includeSecrets = false\) => request<[^\n]*\/settings\/export\$\{includeSecrets \? '\?include_secrets=true' : ''\}/)
  for (const locale of ['zh', 'en', 'zh-TW']) {
    const messages = JSON.parse(readFileSync(new URL(`../locales/${locale}.json`, import.meta.url), 'utf8'))
    for (const key of ['title', 'description', 'export', 'import', 'scope', 'confirm', 'invalid_format', 'invalid_field', 'invalid_type', 'failedSection',
      'includeSecrets', 'secretsWarningTitle', 'secretsWarning', 'secretsWarningConfirm', 'exportHintSecrets', 'exportedWithSecrets', 'fileHasSecrets', 'fileWithoutSecrets']) {
      assert.equal(typeof messages.settings.transfer[key], 'string', `${locale} settings.transfer.${key}`)
    }
  }
})

test('exports omit secrets unless opted in behind a warning, and import accepts both kinds of file', () => {
  const secretFree = { ...target, secrets_included: false, settings: { site_name: 'target', global_rpm: 100 } }
  const withSecrets = { ...target, secrets_included: true, settings: { global_rpm: 7, github_token: 'ghp-test-only', prompt_filter_review_api_key: 'sk-test-only', image_s3_access_key: 'ak', image_s3_secret_key: 'sk' } }
  const referenceWithoutSecrets = { ...target, settings: { site_name: 'target', global_rpm: 100 } }
  assert.deepEqual(parseSettingsBackup(JSON.stringify(secretFree), referenceWithoutSecrets), secretFree)
  assert.deepEqual(parseSettingsBackup(JSON.stringify(withSecrets), referenceWithoutSecrets), withSecrets)
  for (const bad of [
    { ...withSecrets, settings: { github_token: 42 } },
    { ...withSecrets, secrets_included: 'yes' },
    { ...withSecrets, sections: { invite_guide: { github_token: 'x' } } },
  ]) assert.throws(() => parseSettingsBackup(JSON.stringify(bad), referenceWithoutSecrets), SettingsImportError, JSON.stringify(bad))
  const server = readFileSync(new URL('../../../admin/settings_export.go', import.meta.url), 'utf8')
  const serverFields = server.match(/settingsExportSecretFields = \[\]string\{([^}]*)\}/)[1].match(/"([a-z0-9_]+)"/g).map(field => field.slice(1, -1))
  assert.deepEqual([...SETTINGS_SECRET_FIELDS].sort(), serverFields.sort(), 'frontend and server agree on secret fields')

  const component = readFileSync(new URL('../components/SettingsTransfer.tsx', import.meta.url), 'utf8')
  assert.match(component, /from '\.\/ui\/switch'/)
  assert.match(component, /useState\(false\)\n  const \{ confirm, confirmDialog \} = useConfirmDialog\(\)/, 'include secrets starts off')
  assert.match(component, /if \(next && !await confirm\(\{/, 'turning secrets on asks for confirmation')
  assert.match(component, /readSettingsBackup\(includeSecrets\)/)
  assert.match(component, /readSettingsBackup\(\)\]\)/, 'the import reference is a secret-free export')
})
