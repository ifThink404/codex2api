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
    for (const key of ['autoLock', 'lockedSessions', 'lockedSessionsEmpty', 'unlock', 'unlocked', 'sessionPrefix', 'lockedAt', 'vault']) {
      assert.equal(typeof locale.runtime?.[key], 'string', `${name}.json runtime.${key} missing`)
    }
  }
})
