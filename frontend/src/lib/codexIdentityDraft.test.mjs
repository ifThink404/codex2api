import test from 'node:test'
import assert from 'node:assert/strict'
import {
  parseCodexUserAgentConfig, serializeCodexUserAgentConfig,
  switchCodexIdentityDraft, updateCodexIdentityDraft, reconcileCodexIdentitySave,
} from './codexIdentityDraft.ts'

test('Word BPS UA survives native profile switches and saving without changing native UA', () => {
  let draft = parseCodexUserAgentConfig('{"client_kind":"codex-desktop","raw_user_agent":"Native/1","bps_word_user_agent":"Word/1"}')
  draft = switchCodexIdentityDraft(draft, 'codex-tui')
  draft = updateCodexIdentityDraft(draft, { bps_word_user_agent: 'Word/2' })
  const saved = parseCodexUserAgentConfig(serializeCodexUserAgentConfig(draft))
  assert.equal(saved.bps_word_user_agent, 'Word/2')
  assert.equal(switchCodexIdentityDraft(saved, 'codex-desktop').raw_user_agent, 'Native/1')
  assert.equal(saved.raw_user_agent, undefined)
})

test('switching away and back restores all fields, including raw overrides, before saving', () => {
  const original = { client_kind: 'codex-tui', client_version: '0.155.0', raw_user_agent: 'CLI/custom', os_name: 'Windows', terminal: 'WindowsTerminal' }
  let draft = switchCodexIdentityDraft(original, 'codex-desktop')
  assert.equal(draft.raw_user_agent, undefined)
  draft = updateCodexIdentityDraft(draft, { client_version: '0.154.1', app_version: '26.1.2', os_name: 'Mac OS' })
  const cli = switchCodexIdentityDraft(draft, 'codex-tui')
  for (const [key, value] of Object.entries(original)) assert.equal(cli[key], value)
  const desktop = switchCodexIdentityDraft(cli, 'codex-desktop')
  assert.equal(desktop.client_version, '0.154.1')
  assert.equal(desktop.app_version, '26.1.2')
  assert.equal(desktop.os_name, 'Mac OS')
  assert.deepEqual(original, { client_kind: 'codex-tui', client_version: '0.155.0', raw_user_agent: 'CLI/custom', os_name: 'Windows', terminal: 'WindowsTerminal' })
})

test('all modes and the custom fallback survive saving and reloading profiles', () => {
  let draft = parseCodexUserAgentConfig('{"client_name":"codex_vscode","client_version":"0.155.0"}')
  draft = switchCodexIdentityDraft(draft, 'custom')
  draft = updateCodexIdentityDraft(draft, { client_name: 'Fallback Client', raw_user_agent: 'Fallback Client/0.155.0', mode: 'multi' })
  draft = switchCodexIdentityDraft(draft, 'codex-exec')
  draft = updateCodexIdentityDraft(draft, { terminal: 'dumb', mode: 'pool', pool_mix: { 'codex-tui': 10 } })
  draft = updateCodexIdentityDraft(draft, { mode: '' })
  const saved = parseCodexUserAgentConfig(serializeCodexUserAgentConfig(draft))
  assert.equal(switchCodexIdentityDraft(saved, 'custom').raw_user_agent, 'Fallback Client/0.155.0')
  assert.equal(switchCodexIdentityDraft(saved, 'codex-vscode').client_version, '0.155.0')
  assert.equal(switchCodexIdentityDraft(saved, 'codex-exec').terminal, 'dumb')
  assert.deepEqual(saved.pool_mix, { 'codex-tui': 10 })
})

test('typing and switching retain unfinished values and spaces', () => {
  let draft = updateCodexIdentityDraft({}, { os_name: 'Mac ', client_version: '0.155.' })
  draft = switchCodexIdentityDraft(switchCodexIdentityDraft(draft, 'custom'), 'codex-tui')
  draft = parseCodexUserAgentConfig(serializeCodexUserAgentConfig(draft))
  assert.equal(draft.os_name, 'Mac ')
  assert.equal(draft.client_version, '0.155.')
})

test('a pending save cannot overwrite edits or profile switches made after submission', () => {
  const submitted = serializeCodexUserAgentConfig({ client_kind: 'codex-tui', client_version: 'v0.155.0' })
  const normalized = serializeCodexUserAgentConfig({ client_kind: 'codex-tui', client_version: '0.155.0' })
  const newer = serializeCodexUserAgentConfig(switchCodexIdentityDraft(parseCodexUserAgentConfig(submitted), 'custom'))
  assert.equal(reconcileCodexIdentitySave(submitted, submitted, normalized), normalized)
  assert.equal(reconcileCodexIdentitySave(newer, submitted, normalized), newer)
})
