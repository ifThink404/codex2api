import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import { fileURLToPath } from 'node:url'
import { formStateFromAccount, buildQuickConfigSavePayload } from './accountQuickConfig.ts'
import { bpsFormFromAccount, bpsPayloadFromForm, isBPSAccount, parseRouteModels } from './bpsAccount.ts'

const srcRoot = fileURLToPath(new URL('..', import.meta.url))
const read = path => readFileSync(srcRoot + path, 'utf8')

test('quick configuration round trips the tri-state BPS override', () => {
  for (const [value, expected] of [[true, true], [false, false], [null, null], [undefined, null]]) {
    const form = formStateFromAccount({ id: 1, codex_bps_enabled: value })
    const payload = buildQuickConfigSavePayload(form, true).payload
    assert.equal(payload.codex_bps_enabled, expected)
    assert.equal('codex_bps_profile' in payload, false, 'quick configuration only sends the switch')
  }
})

test('account dialog sends every BPS field and defaults to inherit / Word / off', () => {
  const empty = bpsFormFromAccount({})
  assert.deepEqual([empty.enabled, empty.native, empty.profile, empty.convergence, empty.imageTrim], ['inherit', 'inherit', 'word', 'off', false])
  const form = bpsFormFromAccount({ codex_bps_enabled: true, codex_native_enabled: false, codex_bps_models: ['gpt-6-*'], codex_bps_profile: 'excel', codex_bps_convergence: 'turn_round', codex_bps_image_trim_enabled: true })
  assert.deepEqual(bpsPayloadFromForm(form), {
    codex_bps_enabled: true, codex_native_enabled: false, codex_native_models: [], codex_bps_models: ['gpt-6-*'],
    codex_bps_image_trim_enabled: true, codex_bps_profile: 'excel', codex_bps_convergence: 'turn_round',
  })
  assert.equal(bpsFormFromAccount({ codex_bps_profile: 'visio', codex_bps_convergence: 'device' }).profile, 'word')
  assert.deepEqual(parseRouteModels(' a, b\nc '), ['a', 'b', 'c'])
})

test('BPS eligibility excludes relay, Grok, Claude, Antigravity and agent identity', () => {
  assert.equal(isBPSAccount({}), true)
  for (const flag of ['openai_responses_api', 'grok_api', 'claude_api', 'antigravity_api', 'agent_identity']) {
    assert.equal(isBPSAccount({ [flag]: true }), false, flag)
  }
})

test('BPS account controls use shared components, the scheduler API and i18n', () => {
  const fields = read('components/BPSAccountFields.tsx')
  for (const component of ["from '@/components/ui/select'", "from '@/components/ui/switch'", "from '@/components/ui/input'"]) {
    assert.ok(fields.includes(component), component)
  }
  assert.doesNotMatch(fields, /<select[\s>]|type="checkbox"/)
  assert.doesNotMatch(fields, /[一-鿿]/, 'no hard-coded Chinese; strings go through t()')
  const accounts = read('pages/Accounts.tsx')
  assert.ok(accounts.includes('bpsPayloadFromForm(editBPS)'), 'edit dialog saves BPS fields')
  assert.ok(accounts.includes('account.codex_bps_active &&'), 'row badge shows effective BPS')
  assert.ok(read('components/AccountQuickConfigSheet.tsx').includes('<BPSAccountFields compact'), 'quick config keeps the switch')
  assert.ok(read('components/TestConnectionModal.tsx').includes('params.set("test_mode", testMode)'), 'connection test path')
  const keys = ['title', 'enabled', 'enabledHelp', 'on', 'off', 'inheritPlugin', 'inheritNative', 'activeNow', 'inactiveNow', 'profile', 'convergence', 'native', 'imageTrim', 'bpsModels', 'nativeModels', 'modelsPlaceholder', 'routesHelp', 'badge', 'testMode']
  for (const lang of ['zh', 'en', 'zh-TW']) {
    const bps = JSON.parse(read(`locales/${lang}.json`)).accounts.bps
    for (const key of keys) assert.ok(bps[key], `${lang}: accounts.bps.${key}`)
    for (const profile of ['word', 'excel', 'sheets', 'powerpoint']) assert.ok(bps.profiles[profile], `${lang} profile ${profile}`)
    for (const mode of ['off', 'session', 'full', 'round', 'turn_round']) assert.ok(bps.convergenceModes[mode], `${lang} mode ${mode}`)
    for (const mode of ['auto', 'codex', 'bps']) assert.ok(bps.testModes[mode], `${lang} test mode ${mode}`)
  }
})
