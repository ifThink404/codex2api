import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const settingsSource = readFileSync(new URL('../pages/Settings.tsx', import.meta.url), 'utf8')
const typesSource = readFileSync(new URL('../types.ts', import.meta.url), 'utf8')
const locales = Object.fromEntries(
  ['zh', 'en', 'zh-TW'].map((name) => [name, JSON.parse(readFileSync(new URL(`../locales/${name}.json`, import.meta.url), 'utf8'))]),
)

const SETTING_KEYS = [
  'codex_turn_state_strict',
  'codex_session_no_borrow_enabled',
  'codex_session_no_borrow_hold_seconds',
  'codex_initial_session_admission_enabled',
  'codex_initial_session_max_age_seconds',
]

const I18N_KEYS = [
  'codexTurnStateStrict', 'codexTurnStateStrictDesc',
  'codexSessionNoBorrow', 'codexSessionNoBorrowDesc',
  'codexSessionNoBorrowHoldSeconds', 'codexSessionNoBorrowHoldSecondsDesc',
  'codexInitialSessionAdmission', 'codexInitialSessionAdmissionDesc',
  'codexInitialSessionMaxAge', 'codexInitialSessionMaxAgeDesc',
]

test('session guard settings are typed, defaulted and rendered with shared controls', () => {
  for (const key of SETTING_KEYS) {
    assert.ok(typesSource.includes(`${key}:`) || typesSource.includes(`${key}?:`), `types.ts lacks ${key}`)
    assert.ok(settingsSource.includes(`${key}:`), `Settings.tsx form defaults lack ${key}`)
  }
  assert.ok(settingsSource.includes("autoSaveBooleanField('codex_turn_state_strict'"), 'turn-state strict switch missing')
  assert.ok(settingsSource.includes("autoSaveBooleanField('codex_session_no_borrow_enabled'"), 'no-borrow switch missing')
  assert.ok(settingsSource.includes('autoSaveSettingsPatch({ codex_session_no_borrow_hold_seconds: value })'), 'no-borrow hold input missing')
  assert.ok(settingsSource.includes("autoSaveBooleanField('codex_initial_session_admission_enabled'"), 'admission switch missing')
  assert.ok(settingsSource.includes('autoSaveSettingsPatch({ codex_initial_session_max_age_seconds: value })'), 'max age input missing')
  assert.equal(/<select[\s>]/.test(settingsSource.slice(settingsSource.indexOf('codex_turn_state_strict'))), false, 'no hand-written <select>')
})

test('session guard copy exists in every locale', () => {
  for (const [name, locale] of Object.entries(locales)) {
    for (const key of I18N_KEYS) {
      assert.equal(typeof locale.settings?.[key], 'string', `${name}.json settings.${key} missing`)
    }
  }
})

const runtimeSource = readFileSync(new URL('../pages/RuntimeStatus.tsx', import.meta.url), 'utf8')

test('runtime status renders the session guards panel with shared StatusPanel', () => {
  assert.ok(runtimeSource.includes("t('runtime.sessionGuards')"), 'panel title missing')
  assert.ok(runtimeSource.includes('status.session_guards.turn_state.totals'), 'turn-state totals row missing')
  assert.ok(runtimeSource.includes('status.session_guards.borrow.borrowed'), 'borrow row missing')
  assert.ok(runtimeSource.includes('status.session_guards.initial_session.recent_hour'), 'initial session row missing')
  assert.ok(typesSource.includes('session_guards?:'), 'RuntimeStatusResponse.session_guards missing')
  for (const [name, locale] of Object.entries(locales)) {
    for (const key of ['sessionGuards', 'turnStateTotals', 'sessionBorrow', 'initialSessionRecentHour', 'noSamples']) {
      assert.equal(typeof locale.runtime?.[key], 'string', `${name}.json runtime.${key} missing`)
    }
  }
})

test('auto-lock and vault settings, locked-sessions card and copy exist', () => {
  for (const key of ['codex_session_auto_lock_enabled', 'codex_session_auto_lock_threshold', 'codex_turn_state_vault_enabled']) {
    assert.ok(typesSource.includes(`${key}?:`), `types.ts lacks ${key}`)
    assert.ok(settingsSource.includes(`${key}:`), `Settings.tsx defaults lack ${key}`)
  }
  assert.ok(settingsSource.includes("autoSaveBooleanField('codex_session_auto_lock_enabled'"))
  assert.ok(settingsSource.includes('autoSaveSettingsPatch({ codex_session_auto_lock_threshold: value })'))
  assert.ok(settingsSource.includes("autoSaveBooleanField('codex_turn_state_vault_enabled'"))
  assert.ok(runtimeSource.includes('api.getSessionLocks'), 'locked sessions must be loaded')
  assert.ok(runtimeSource.includes('api.deleteSessionLock('), 'unlock action missing')
  assert.ok(runtimeSource.includes('status.session_guards.auto_lock'), 'auto-lock row missing')
  assert.ok(typesSource.includes('export interface SessionLockItem'))
  for (const [name, locale] of Object.entries(locales)) {
    for (const key of ['codexSessionAutoLock', 'codexSessionAutoLockDesc', 'codexSessionAutoLockThreshold', 'codexSessionAutoLockThresholdDesc', 'codexTurnStateVault', 'codexTurnStateVaultDesc']) {
      assert.equal(typeof locale.settings?.[key], 'string', `${name}.json settings.${key} missing`)
    }
    for (const key of ['autoLock', 'lockedSessions', 'lockedSessionsEmpty', 'unlock', 'sessionPrefix', 'lockedAt', 'vault']) {
      assert.equal(typeof locale.runtime?.[key], 'string', `${name}.json runtime.${key} missing`)
    }
  }
})

// DESIGN.md:40 要求新增设置区块断言「用了哪个共享组件」：只断言 API 方法的话，把开关
// 换成裸 <input type="checkbox">、把数字输入换成裸 <input type="number"> 仍然全绿。
function settingFieldBlock(source, labelKey) {
  const at = source.indexOf(`t('settings.${labelKey}')`)
  assert.notEqual(at, -1, `Settings.tsx lacks the ${labelKey} field`)
  const end = source.indexOf('</SettingField>', at)
  assert.notEqual(end, -1, `the ${labelKey} field is never closed`)
  return source.slice(at, end)
}

test('auto-lock and vault settings render with the shared controls', () => {
  const autoLock = settingFieldBlock(settingsSource, 'codexSessionAutoLock')
  assert.ok(autoLock.includes('<Switch'), 'the auto-lock toggle must be components/ui/switch')
  assert.ok(autoLock.includes("autoSaveBooleanField('codex_session_auto_lock_enabled'"), 'the auto-lock toggle must autosave')

  const threshold = settingFieldBlock(settingsSource, 'codexSessionAutoLockThreshold')
  assert.ok(threshold.includes('<DraftNumberInput'), 'the threshold must be components/ui/draft-number-input')
  assert.ok(threshold.includes('min={1}') && threshold.includes('max={10000}'), 'the threshold must keep its 1–10000 bounds')
  assert.ok(threshold.includes('disabled={!settingsForm.codex_session_auto_lock_enabled}'), 'the threshold must grey out when the toggle is off')

  const vault = settingFieldBlock(settingsSource, 'codexTurnStateVault')
  assert.ok(vault.includes('<Switch'), 'the vault toggle must be components/ui/switch')
  assert.ok(vault.includes("autoSaveBooleanField('codex_turn_state_vault_enabled'"), 'the vault toggle must autosave')

  assert.equal(/<input\s+type="checkbox"/.test(settingsSource), false, 'no raw checkbox anywhere in Settings.tsx')
  assert.equal(/<select[\s>]/.test(settingsSource), false, 'no hand-written <select> anywhere in Settings.tsx')
})

test('locked-sessions card uses shared components, the table shell and error copy', () => {
  for (const specifier of ['@/components/ui/table', '@/components/ui/button', '@/components/ui/card']) {
    assert.ok(runtimeSource.includes(`from '${specifier}'`), `RuntimeStatus.tsx must use the shared components from ${specifier}`)
  }
  assert.ok(runtimeSource.includes('lg:col-span-2'), 'the locked-sessions card must span the full grid row')
  assert.ok(runtimeSource.includes('data-table-shell'), 'the locks table must sit in a data-table-shell (border, sticky header, own scroll)')
  assert.match(runtimeSource, /finally\s*\{\s*await reloadLocks\(\)/, 'unlock must reload the locks even when the request fails')
  assert.ok(runtimeSource.includes("t('runtime.locksLoadFailed')"), 'a failed locks fetch must not render as "no locked sessions"')
  assert.ok(runtimeSource.includes("t('runtime.unlockFailed')"), 'a failed unlock must surface in the card, not just in the console')
  assert.equal(/<input\s+type="checkbox"/.test(runtimeSource), false, 'no raw checkbox in RuntimeStatus.tsx')
  assert.equal(/<select[\s>]/.test(runtimeSource), false, 'no hand-written <select> in RuntimeStatus.tsx')
  for (const [name, locale] of Object.entries(locales)) {
    for (const key of ['locksLoadFailed', 'unlockFailed']) {
      assert.equal(typeof locale.runtime?.[key], 'string', `${name}.json runtime.${key} missing`)
    }
  }
})
