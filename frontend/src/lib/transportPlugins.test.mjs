import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import { fileURLToPath } from 'node:url'
import { CAPTURE_PURGE_MODES, PLUGIN_COOLING_REASONS, PLUGIN_VIEWS, formatCaptureBytes, bpsConfigFields, normalizePluginConfig, pluginConfigListText, pluginCoolingReasonKey, captureIdFromEvidence, pluginConfigBoolean, pluginCaptureAgentFilters, pluginCaptureSource, normalizePluginView, parsePluginConfigText, pluginMetaSummary, sampleRateFromPercent, sampleRateToPercent } from './transportPlugins.ts'

const srcRoot = fileURLToPath(new URL('..', import.meta.url))
const read = path => readFileSync(srcRoot + path, 'utf8')

test('plugin views and helpers', () => {
  assert.deepEqual([...PLUGIN_VIEWS], ['overview', 'captures', 'logs', 'errors', 'agent'])
  assert.equal(normalizePluginView('captures'), 'captures')
  assert.equal(normalizePluginView('nope'), 'overview')
  assert.equal(normalizePluginView(undefined), 'overview')
  assert.equal(sampleRateToPercent(0.25), 25)
  assert.equal(sampleRateToPercent(3), 100)
  assert.equal(sampleRateFromPercent(12.5), 0.125)
  assert.equal(sampleRateFromPercent(-1), 0)
  assert.deepEqual(parsePluginConfigText('{"a":1}'), { a: 1 })
  assert.deepEqual(parsePluginConfigText(''), {})
  assert.equal(parsePluginConfigText('[1]'), null)
  assert.equal(parsePluginConfigText('{'), null)
  assert.equal(pluginMetaSummary('{"profile":"word","agent_iteration":"3","nested":{}}'), 'profile=word · agent_iteration=3')
  assert.equal(pluginMetaSummary(''), '')
})

test('BPS config form covers every server config key', () => {
  const server = readFileSync(srcRoot + '../../proxy/bps_plugin.go', 'utf8')
  const keys = [...server.matchAll(/json:"([a-z0-9_]+),omitempty"`/g)].map(match => match[1])
  const configBlock = server.slice(server.indexOf('type BPSConfig struct'), server.indexOf('func (c BPSConfig) normalized'))
  const serverKeys = keys.filter(key => configBlock.includes(`json:"${key},`))
  assert.deepEqual(bpsConfigFields.map(field => field.key).sort(), serverKeys.sort())
})

test('boolean config fields show the server default when absent', () => {
  const exclude = bpsConfigFields.find(field => field.key === 'exclude_failures_from_native_health')
  const fallback = bpsConfigFields.find(field => field.key === 'attachment_429_fallback')
  assert.equal(pluginConfigBoolean({}, exclude), true, 'native-health exclusion defaults on, as on the server')
  assert.equal(pluginConfigBoolean({ exclude_failures_from_native_health: false }, exclude), false)
  assert.equal(pluginConfigBoolean({}, fallback), false, 'the 429 fallback defaults off')
  const persist = bpsConfigFields.find(field => field.key === 'persist_heuristic_affinity')
  assert.equal(pluginConfigBoolean({}, persist), true, 'heuristic affinity persists by default, as in fj')
  assert.ok(exclude.hint && persist.hint, 'both tradeoff switches explain themselves')
  const server = readFileSync(srcRoot + '../../proxy/bps_plugin.go', 'utf8')
  assert.match(server, /ExcludeFailuresFromNativeHealth == nil \|\| \*c\.ExcludeFailuresFromNativeHealth/)
})

test('plugin pages are routed, in the nav, and use shared components and the plugin API', () => {
  const app = read('App.tsx')
  for (const route of ['path="/plugins"', 'path="/plugins/:id/:view"']) assert.ok(app.includes(route), route)
  assert.ok(read('components/Layout.tsx').includes("labelKey: 'nav.plugins'"))
  const page = read('pages/Plugins.tsx')
  assert.doesNotMatch(page, /<select[\s>]|type="checkbox"|<input[\s>]/)
  assert.doesNotMatch(page, /[一-鿿]/, 'strings go through t()')
  for (const needle of [
    "from '@/components/ui/select'", "from '@/components/ui/switch'", "from '@/components/ui/draft-number-input'",
    "import AccountGroupMultiSelect", '<OperationsErrors transport={plugin.id} embedded />',
    'api.updateTransportPlugin', 'api.setTransportPluginAccountOverride', 'api.getPluginCaptures', 'api.getPluginCapture(',
    'transport: plugin.id',
  ]) assert.ok(page.includes(needle), needle)
  const errors = read('pages/OperationsErrors.tsx')
  assert.ok(errors.includes('transport,') && errors.includes('{!embedded && <OpsTabs />}'))
})

test('plugin i18n keys exist in zh, en and zh-TW', () => {
  const page = read('pages/Plugins.tsx')
  const used = new Set([...page.matchAll(/t\('plugins\.([a-zA-Z.]+)'/g)].map(match => match[1]))
  for (const view of PLUGIN_VIEWS) used.add(`views.${view}`)
  for (const dir of ['request', 'upstream_request', 'response', 'error']) used.add(`directions.${dir}`)
  for (const reason of [...PLUGIN_COOLING_REASONS, 'unknown']) used.add(`coolingReasons.${reason}`)
  for (const mode of CAPTURE_PURGE_MODES) used.add(`purgeModes.${mode}`)
  for (const mode of ['errors_only', 'all']) used.add(`purgeConfirm_${mode}`)
  for (const field of bpsConfigFields) {
    used.add(`bpsConfig.${field.key}`)
    if (field.hint) used.add(`bpsConfigHints.${field.key}`)
  }
  for (const lang of ['zh', 'en', 'zh-TW']) {
    const locale = JSON.parse(read(`locales/${lang}.json`))
    assert.ok(locale.nav.plugins, `${lang} nav.plugins`)
    for (const key of used) {
      const value = key.split('.').reduce((node, part) => node?.[part], locale.plugins)
      assert.equal(typeof value, 'string', `${lang}: plugins.${key}`)
    }
  }
})

test('batch connection test sends the chosen test path', () => {
  const accounts = read('pages/Accounts.tsx')
  assert.ok(accounts.includes('test_mode: batchTestMode'))
  assert.ok(accounts.includes('value={batchTestMode}'))
})

test('plugin log-agent wiring', () => {
  assert.equal(pluginCaptureSource('bps'), 'bps.captures')
  assert.equal(captureIdFromEvidence('cap:42'), 42)
  assert.equal(captureIdFromEvidence('usage:42'), null)
  assert.deepEqual(pluginCaptureAgentFilters({ requestId: ' r1 ', accountId: 'x', status: '429', direction: 'error' }), { request_id: 'r1', status: '429', direction: 'error' })
  const page = read('pages/Plugins.tsx')
  assert.ok(page.includes('source={pluginCaptureSource(plugin.id)}'), 'captures and agent views use the capture source')
  assert.ok(page.includes('source="ops_errors"') && page.includes('filters={{ transport: plugin.id }}'), 'agent view analyses the plugin errors')
  assert.ok(page.includes('refs={[detail.request_id]}'), 'capture detail analyses one request')
  assert.ok(read('lib/logAgent.ts').includes('transport: params.transport'), 'embedded errors view passes its transport to the agent')
})

test('the BPS account list shows plugin cooldowns with their reason', () => {
  const page = read('pages/Plugins.tsx')
  assert.ok(page.includes('api.getPluginAccountStatus(plugin.id'))
  assert.ok(page.includes('<PluginAccountStatusLine status={statuses.get(account.id)} />'))
  assert.equal(pluginCoolingReasonKey('bps_rate_limited'), 'plugins.coolingReasons.bps_rate_limited')
  assert.equal(pluginCoolingReasonKey(undefined), 'plugins.coolingReasons.unknown')
  const server = readFileSync(srcRoot + '../../proxy/bps_account_state.go', 'utf8')
  for (const reason of PLUGIN_COOLING_REASONS) assert.ok(server.includes(`"${reason}"`), `server reports ${reason}`)
})

test('model list fields edit as comma-separated text and save as arrays with server defaults', () => {
  const models = bpsConfigFields.find(field => field.key === 'bps_models')
  const only = bpsConfigFields.find(field => field.key === 'bps_only_models')
  assert.deepEqual(models.defaultValue, ['gpt-5.6-*', 'gpt-6-*'])
  assert.deepEqual(only.defaultValue, ['gpt-6-*'])
  const server = readFileSync(srcRoot + '../../proxy/bps_plugin.go', 'utf8')
  assert.ok(server.includes('defaultBPSModels     = []string{"gpt-5.6-*", "gpt-6-*"}'), 'defaults match the server')
  assert.ok(server.includes('defaultBPSOnlyModels = []string{"gpt-6-*"}'))
  const ladder = bpsConfigFields.find(field => field.key === 'bps_policy_cooldown_ladder')
  assert.deepEqual(ladder.defaultValue, ['2m', '10m', '30m', '2h'])
  assert.ok(server.includes('defaultBPSPolicyCooldownLadder = []string{"2m", "10m", "30m", "2h"}'), 'ladder default matches the server')
  assert.ok(read('pages/Plugins.tsx').includes("t('plugins.policyTier'"))
  assert.equal(pluginConfigListText({ bps_models: ['gpt-6-*', 'gpt-5.6-*'] }, 'bps_models'), 'gpt-6-*, gpt-5.6-*')
  assert.equal(pluginConfigListText({ bps_models: 'gpt-6-*,' }, 'bps_models'), 'gpt-6-*,', 'drafts keep the typed text')
  assert.deepEqual(normalizePluginConfig({ bps_models: ' gpt-6-* , ,gpt-5.6-sol ', bps_only_models: '  ', word_user_agent: 'x' }, bpsConfigFields), { bps_models: ['gpt-6-*', 'gpt-5.6-sol'], word_user_agent: 'x' })
  const page = read('pages/Plugins.tsx')
  assert.ok(page.includes('normalizePluginConfig(config, typedFields)'))
  assert.ok(page.includes("t('plugins.modelUnavailable'"))
})

test('captures page shows storage and purges with a mode and a confirmation', () => {
  const page = read('pages/Plugins.tsx')
  for (const needle of [
    '<CaptureCleanup plugin={plugin}', 'api.getPluginCaptureStats(plugin.id)', 'api.purgePluginCaptures(plugin.id, mode',
    "if (!await confirm({ title: t('plugins.purgeTitle')", "tone: 'destructive'",
  ]) assert.ok(page.includes(needle), needle)
  assert.deepEqual([...CAPTURE_PURGE_MODES].sort(), ['all', 'errors_only', 'older_than'])
  const server = readFileSync(srcRoot + '../../database/plugin_captures.go', 'utf8')
  for (const mode of CAPTURE_PURGE_MODES) assert.ok(server.includes(`= "${mode}"`), `server purge mode ${mode}`)
  assert.equal(formatCaptureBytes(512), '512 B')
  assert.equal(formatCaptureBytes(291 * 1024 * 1024), '291 MB')
  assert.equal(formatCaptureBytes(1536), '1.5 KB')
})

test('BPS account list shows concurrency and request budget usage', () => {
  const page = read('pages/Plugins.tsx')
  assert.ok(page.includes("t('plugins.inFlight'"))
  assert.ok(page.includes("t('plugins.budgetUsed', { used: status.budget_used ?? 0, budget: status.budget })"))
  for (const key of ['bps_account_max_concurrency', 'bps_account_request_budget', 'bps_account_budget_window']) {
    assert.ok(bpsConfigFields.some(field => field.key === key && field.hint), key)
  }
})

test('the BPS account list shows usage-policy probe state', () => {
  const page = read('pages/Plugins.tsx')
  assert.ok(page.includes("t('plugins.probePending')"))
  assert.ok(page.includes("t('plugins.lastProbe', { time: formatBeijingTime(status.last_probe), result: status.last_probe_result })"))
})
