import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import { fileURLToPath } from 'node:url'
import { formStateFromAccount, buildQuickConfigSavePayload } from './accountQuickConfig.ts'
import { batchBPSPayload, bpsFormFromAccount, bpsPayloadFromForm, emptyBatchBPSForm, isBPSAccount, parseRouteModels } from './bpsAccount.ts'
import { buildBatchMetadataUpdate } from './accountBatchUpdate.ts'

const srcRoot = fileURLToPath(new URL('..', import.meta.url))
const read = path => readFileSync(srcRoot + path, 'utf8')

test('quick configuration round trips the tri-state BPS override', () => {
  for (const [value, expected] of [[true, true], [false, false], [null, null], [undefined, null]]) {
    // The loaded switch is left alone; a changed one is sent as the tri-state.
    const loaded = formStateFromAccount({ id: 1, codex_bps_enabled: value })
    assert.equal('codex_bps_enabled' in buildQuickConfigSavePayload(loaded, true).payload, false)
    const other = formStateFromAccount({ id: 1, codex_bps_enabled: expected === true ? false : true })
    const form = { ...other, bps: { ...other.bps, enabled: expected === true ? 'on' : expected === false ? 'off' : 'inherit' } }
    const payload = buildQuickConfigSavePayload(form, true).payload
    assert.equal(payload.codex_bps_enabled, expected)
    assert.equal('codex_bps_profile' in payload, false, 'quick configuration only sends the switch')
  }
})

test('account dialog sends every BPS field and defaults to inherit / Word / off', () => {
  const empty = bpsFormFromAccount({})
  assert.deepEqual([empty.enabled, empty.native, empty.profile, empty.convergence, empty.imageTrim], ['inherit', 'inherit', 'word', 'off', 'inherit'])
  const form = bpsFormFromAccount({ codex_bps_enabled: true, codex_native_enabled: false, codex_bps_models: ['gpt-6-*'], codex_bps_profile: 'excel', codex_bps_convergence: 'turn_round', codex_bps_image_trim_enabled: true })
  assert.deepEqual(bpsPayloadFromForm(form), {
    codex_bps_enabled: true, codex_native_enabled: false, codex_native_models: [], codex_bps_models: ['gpt-6-*'],
    codex_bps_image_trim_enabled: true, codex_bps_profile: 'excel', codex_bps_convergence: 'turn_round',
  })
  assert.equal(bpsFormFromAccount({ codex_bps_profile: 'visio', codex_bps_convergence: 'device' }).profile, 'word')
  assert.equal(bpsPayloadFromForm(bpsFormFromAccount({})).codex_bps_image_trim_enabled, null, 'an unset image trim stays unset (plugin default)')
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
  for (const component of ["from '@/components/ui/select'", "from '@/components/ui/input'"]) {
    assert.ok(fields.includes(component), component)
  }
  assert.doesNotMatch(fields, /<select[\s>]|type="checkbox"/)
  assert.doesNotMatch(fields, /[一-鿿]/, 'no hard-coded Chinese; strings go through t()')
  const accounts = read('pages/Accounts.tsx')
  assert.ok(accounts.includes('bpsPayloadFromForm(editBPS)'), 'edit dialog saves BPS fields')
  assert.ok(accounts.includes('account.codex_bps_active &&'), 'row badge shows effective BPS')
  assert.ok(read('components/AccountQuickConfigSheet.tsx').includes('<BPSAccountFields compact'), 'quick config keeps the switch')
  assert.ok(read('components/TestConnectionModal.tsx').includes('params.set("test_mode", testMode)'), 'connection test path')
  const keys = ['title', 'enabled', 'enabledHelp', 'on', 'off', 'inheritPlugin', 'inheritNative', 'activeNow', 'inactiveNow', 'profile', 'convergence', 'native', 'imageTrim', 'inheritImageTrim', 'bpsModels', 'nativeModels', 'modelsPlaceholder', 'routesHelp', 'badge', 'testMode']
  for (const lang of ['zh', 'en', 'zh-TW']) {
    const bps = JSON.parse(read(`locales/${lang}.json`)).accounts.bps
    for (const key of keys) assert.ok(bps[key], `${lang}: accounts.bps.${key}`)
    for (const profile of ['word', 'excel', 'sheets', 'powerpoint']) assert.ok(bps.profiles[profile], `${lang} profile ${profile}`)
    for (const mode of ['off', 'session', 'full', 'round', 'turn_round']) assert.ok(bps.convergenceModes[mode], `${lang} mode ${mode}`)
    for (const mode of ['auto', 'codex', 'bps']) assert.ok(bps.testModes[mode], `${lang} test mode ${mode}`)
  }
})

test('batch BPS sends only the changed fields', () => {
  assert.deepEqual(batchBPSPayload(emptyBatchBPSForm()), {}, 'everything kept')
  assert.deepEqual(batchBPSPayload({ ...emptyBatchBPSForm(), enabled: 'on', profile: 'excel', native: 'inherit', bpsModels: 'gpt-6-*, gpt-5.6-*', nativeModels: '' }), {
    codex_bps_enabled: true, codex_native_enabled: null, codex_bps_profile: 'excel', codex_bps_models: ['gpt-6-*', 'gpt-5.6-*'], codex_native_models: [],
  })
  assert.deepEqual(batchBPSPayload({ ...emptyBatchBPSForm(), enabled: 'off', imageTrim: 'off', convergence: 'turn_round' }), {
    codex_bps_enabled: false, codex_bps_image_trim_enabled: false, codex_bps_convergence: 'turn_round',
  })
})

test('accounts page: batch BPS, add to group and the group enable path', () => {
  const page = read('pages/Accounts.tsx')
  for (const needle of ['<BatchBPSDialog', 'onClick={() => setShowBatchBPS(true)}', 't("accounts.batchBPS.action")', 'onClick={() => openBatchGroupEditor("addGroups")}', 'addGroups: batchMetaMode === "addGroups"']) {
    assert.ok(page.includes(needle), needle)
  }
  assert.deepEqual(buildBatchMetadataUpdate({ ids: [1], updateTags: false, tags: [], updateGroups: true, groupIds: [7], addGroups: true, updateScoreBias: false, scoreBias: null, updateBaseConcurrency: false, baseConcurrency: null, updateSchedulerPriority: false, schedulerPriority: null }), { ids: [1], add_group_ids: [7] })
  const dialog = read('components/BatchBPSDialog.tsx')
  assert.ok(dialog.includes('api.batchUpdateAccounts({ ids, ...payload })'))
  const plugins = read('pages/Plugins.tsx')
  for (const needle of ['onCreateGroup={createGroup}', "t('plugins.groupsMembers'", '<Link to="/accounts"']) assert.ok(plugins.includes(needle), needle)
  const server = readFileSync(srcRoot + '../../admin/handler.go', 'utf8')
  assert.ok(server.includes('AddGroupIDs *[]int64 `json:"add_group_ids"`'))
  for (const name of ['zh', 'en', 'zh-TW']) {
    const locale = JSON.parse(read(`locales/${name}.json`))
    for (const key of ['action', 'title', 'desc', 'keep', 'replaceScope', 'scopeHint', 'apply', 'done']) assert.equal(typeof locale.accounts.batchBPS[key], 'string', `${name} batchBPS.${key}`)
    for (const key of ['batchGroupAdd', 'batchGroupAddTitle', 'batchGroupAddDesc', 'batchGroupAddFieldHint']) assert.equal(typeof locale.accounts[key], 'string', `${name} ${key}`)
    for (const key of ['groupsMembers', 'groupsNoneEnabled', 'groupsAddAccounts']) assert.equal(typeof locale.plugins[key], 'string', `${name} ${key}`)
  }
})

test('a manual Codex connection test is allowed on BPS accounts', () => {
  const server = readFileSync(srcRoot + '../../proxy/codex_test_mode.go', 'utf8')
  assert.ok(!server.includes('BPSOwnsAccount'), 'no BPS refusal for an explicit Codex test')
  const modal = read('components/TestConnectionModal.tsx')
  assert.ok(modal.includes('(["auto", "codex", "bps"] as CodexTestMode[])'), 'all three test paths are offered')
})

test('accounts page: batch degradation check respects the server queue', () => {
  const page = read('pages/Accounts.tsx')
  for (const needle of ['<BatchDegradeProbeDialog', 'onSelect: () => setShowBatchDegrade(true)', 't("accounts.batchDegrade.done"']) assert.ok(page.includes(needle), needle)
  const dialog = read('components/BatchDegradeProbeDialog.tsx')
  assert.ok(dialog.includes("api.startDegradeProbes('bps', ids, route)"))
  for (const name of ['zh', 'en', 'zh-TW']) {
    const locale = JSON.parse(read(`locales/${name}.json`))
    for (const key of ['action', 'title', 'desc', 'route', 'native', 'start', 'done']) assert.equal(typeof locale.accounts.batchDegrade[key], 'string', `${name} ${key}`)
  }
})
