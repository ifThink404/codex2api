import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import { fileURLToPath } from 'node:url'
import { bpsBadgeView } from './bpsBadge.ts'
import { BPS_ACCOUNT_STATES, degradeVerdictClass, BPS_STATE_BADGE_CLASSES, formatWindowLabel, activityBarPercent, activeUntil, bpsHealthTimeline, bpsTrafficSeries, capacityFill, hasTime, secondsSince, bpsConfigGroups, CAPTURE_PURGE_MODES, formatSuccessRate, PLUGIN_COOLING_REASONS, PLUGIN_VIEWS, formatBlockDuration, formatCaptureBytes, liveElapsedSeconds, secondsUntil, bpsConfigFields, normalizePluginConfig, pluginConfigListText, pluginCoolingReasonKey, captureIdFromEvidence, pluginConfigBoolean, pluginCaptureAgentFilters, pluginCaptureSource, normalizePluginView, parsePluginConfigText, pluginMetaSummary, sampleRateFromPercent, sampleRateToPercent } from './transportPlugins.ts'

const srcRoot = fileURLToPath(new URL('..', import.meta.url))
const read = path => readFileSync(srcRoot + path, 'utf8')
// The Plugins page and the account status line it shares with Accounts.
const readPluginsUI = () => read('pages/Plugins.tsx') + read('components/PluginAccountStatusLine.tsx')

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
  const page = readPluginsUI()
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
  const page = readPluginsUI()
  const used = new Set([...page.matchAll(/t\('plugins\.([a-zA-Z.]+)'/g)].map(match => match[1]))
  for (const view of PLUGIN_VIEWS) used.add(`views.${view}`)
  for (const dir of ['request', 'upstream_request', 'response', 'error']) used.add(`directions.${dir}`)
  for (const reason of [...PLUGIN_COOLING_REASONS, 'unknown']) used.add(`coolingReasons.${reason}`)
  for (const group of bpsConfigGroups) used.add(`bpsConfigGroups.${group.key}.title`).add(`bpsConfigGroups.${group.key}.description`)
  for (const state of BPS_ACCOUNT_STATES) used.add(`dashStates.${state}`)
  for (const key of ['dashRequests', 'dashSuccessRate', 'dashOrg429', 'dashAccount429', 'dashPolicyBlocks', 'dashFirstToken']) used.add(key)
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
  const page = readPluginsUI()
  assert.ok(page.includes('source={pluginCaptureSource(plugin.id)}'), 'captures and agent views use the capture source')
  assert.ok(page.includes('source="ops_errors"') && page.includes('filters={{ transport: plugin.id }}'), 'agent view analyses the plugin errors')
  assert.ok(page.includes('refs={[detail.request_id]}'), 'capture detail analyses one request')
  assert.ok(read('lib/logAgent.ts').includes('transport: params.transport'), 'embedded errors view passes its transport to the agent')
})

test('the BPS account list shows plugin cooldowns with their reason', () => {
  const page = readPluginsUI()
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
  assert.ok(readPluginsUI().includes("t('plugins.policyTier'"))
  assert.equal(pluginConfigListText({ bps_models: ['gpt-6-*', 'gpt-5.6-*'] }, 'bps_models'), 'gpt-6-*, gpt-5.6-*')
  assert.equal(pluginConfigListText({ bps_models: 'gpt-6-*,' }, 'bps_models'), 'gpt-6-*,', 'drafts keep the typed text')
  assert.deepEqual(normalizePluginConfig({ bps_models: ' gpt-6-* , ,gpt-5.6-sol ', bps_only_models: '  ', word_user_agent: 'x' }, bpsConfigFields), { bps_models: ['gpt-6-*', 'gpt-5.6-sol'], word_user_agent: 'x' })
  const page = readPluginsUI()
  assert.ok(page.includes('normalizePluginConfig(config, typedFields)'))
  assert.ok(page.includes("t('plugins.modelUnavailable'"))
})

test('captures page shows storage and purges with a mode and a confirmation', () => {
  const page = readPluginsUI()
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
  const page = readPluginsUI()
  assert.ok(page.includes("t('plugins.inFlight'"))
  assert.ok(page.includes("t('plugins.budgetUsed', { used: status.budget_used ?? 0, budget: status.budget })"))
  for (const key of ['bps_account_max_concurrency', 'bps_account_request_budget', 'bps_account_budget_window']) {
    assert.ok(bpsConfigFields.some(field => field.key === key && field.hint), key)
  }
})

test('the BPS account list shows usage-policy probe state', () => {
  const page = readPluginsUI()
  assert.ok(page.includes("t('plugins.probePending')"))
  assert.ok(page.includes("t('plugins.lastProbe', { time: formatBeijingTime(status.last_probe), result: status.last_probe_result })"))
})

test('policy block durations and live elapsed math', () => {
  assert.equal(formatBlockDuration(42), '42s')
  assert.equal(formatBlockDuration(303), '5m 03s')
  assert.equal(formatBlockDuration(4 * 3600 + 32 * 60), '4h 32m')
  assert.equal(formatBlockDuration(2 * 86400 + 5 * 3600), '2d 5h')
  assert.equal(formatBlockDuration(-5), '0s')
  assert.equal(formatWindowLabel(86400), '24h')
  assert.equal(formatWindowLabel(3600), '1h')
  assert.equal(formatWindowLabel(90), '1m 30s')
  assert.match(BPS_STATE_BADGE_CLASSES.budget_exhausted, /amber/)
  assert.equal(BPS_STATE_BADGE_CLASSES.active, undefined)
  assert.equal(liveElapsedSeconds(5400, 1_000_000, 1_000_000 + 61_500), 5461, 'elapsed ticks forward from the fetch')
  assert.equal(liveElapsedSeconds(5400, 1_000_000, 999_000), 5400, 'never backwards')
  const now = Date.parse('2026-09-29T10:00:00Z')
  assert.equal(secondsUntil('2026-09-29T10:02:00Z', now), 120)
  assert.equal(secondsUntil('2026-09-29T09:00:00Z', now), 0)
  assert.equal(secondsUntil(undefined, now), 0)
})

test('BPS overview has the usage-policy blocks section', () => {
  const page = readPluginsUI()
  for (const needle of [
    "{plugin.id === 'bps' && <PolicyBlocks plugin={plugin} />}", 'api.getPluginPolicyBlocks(plugin.id)',
    'liveElapsedSeconds(block.elapsed_seconds, fetchedAt, now)', 'window.setInterval(() => setNow(Date.now()), 1000)',
  ]) assert.ok(page.includes(needle), needle)
})

test('BPS health dashboard: panels, states, live updates and the server contract', () => {
  const page = readPluginsUI()
  for (const needle of [
    "{plugin.id === 'bps' && <BPSDashboardPanel plugin={plugin} />}", 'api.getBPSDashboard(plugin.id)',
    "if (summary.usable === 0) return 'high'", "return summary.warning ? 'medium' : 'low'", "data.traffic['1h']", 'data?.traffic[range]',
    'liveElapsedSeconds(recovery.longest_active_seconds, fetchedAt, now)',
  ]) assert.ok(page.includes(needle), needle)
  const server = readFileSync(srcRoot + '../../admin/bps_dashboard.go', 'utf8')
  for (const state of BPS_ACCOUNT_STATES) assert.ok(server.includes(`= "${state}"`), `server state ${state}`)
  assert.equal(formatSuccessRate(0.4, 5), '40.0%')
  assert.equal(formatSuccessRate(0.98765, 100), '98.8%')
  assert.equal(formatSuccessRate(0, 0), '—')
})

test('BPS config form is grouped into labeled cards with every field exactly once', () => {
  const grouped = bpsConfigGroups.flatMap(group => group.fields)
  assert.deepEqual([...grouped].sort(), bpsConfigFields.map(field => field.key).sort(), 'every field in a group')
  assert.equal(new Set(grouped).size, grouped.length, 'no field in two groups')
  const page = readPluginsUI()
  for (const needle of [
    "from '../components/SettingsLayout'", '<SettingsCard', 'className={SETTINGS_FIELD_GRID}', 'SETTINGS_ROW_LIST', 'layout="row"',
    'BPS_CONFIG_GROUP_ICONS[group.key]', 'configFieldHint(field)',
  ]) assert.ok(page.includes(needle), needle)
  // Every number field says what 0 means, and the defaults match the server.
  for (const field of bpsConfigFields.filter(field => field.kind === 'number')) {
    assert.ok(field.zeroMeans, `${field.key} states what 0 means`)
    if (field.zeroMeans === 'default') assert.equal(typeof field.defaultValue, 'number', field.key)
  }
  const db = ['bps_plugin.go', 'bps_round_identity.go', 'bps_turn_identity.go'].map(name => readFileSync(srcRoot + '../../database/' + name, 'utf8')).join('\n')
  const serverDefault = name => Number(db.match(new RegExp(`${name}\\s*=\\s*(\\d+)`))[1])
  const want = {
    round_convergence_limit: serverDefault('DefaultBPSRoundConvergenceLimit'),
    round_task_lifetime_hours: serverDefault('DefaultBPSRoundTaskLifetimeHours'),
    turn_round_limit: serverDefault('DefaultBPSTurnRoundLimit'),
    turn_task_lifetime_hours: serverDefault('DefaultBPSTurnTaskLifetimeHours'),
    attachment_request_concurrency: serverDefault('DefaultBPSAttachmentRequestConcurrency'),
    attachment_instance_concurrency: serverDefault('DefaultBPSAttachmentInstanceConcurrency'),
    attachment_account_concurrency: serverDefault('DefaultBPSAttachmentAccountConcurrency'),
  }
  for (const [key, value] of Object.entries(want)) {
    assert.equal(bpsConfigFields.find(field => field.key === key).defaultValue, value, `${key} default matches the server`)
  }
})

test('live activity panel: capacity bars, pagination, sorting source and polling', () => {
  assert.equal(activityBarPercent(3, 10, 99), 30, 'against the cap')
  assert.equal(activityBarPercent(12, 10, 99), 100, 'clamped')
  assert.equal(activityBarPercent(5, 0, 10), 50, 'against the busiest account without a cap')
  assert.equal(activityBarPercent(0, 0, 0), 0)
  assert.equal(capacityFill(0, 10), 'idle')
  assert.equal(capacityFill(4, 10), 'live')
  assert.equal(capacityFill(10, 10), 'over', 'destructive at the cap')
  assert.equal(capacityFill(12, 10), 'over')
  assert.equal(capacityFill(7, 0), 'live', 'no cap is never over')
  const now = Date.parse('2026-09-29T10:00:00Z')
  assert.equal(secondsSince('2026-09-29T09:59:30Z', now), 30)
  assert.equal(secondsSince(undefined, now), undefined)
  const page = readPluginsUI()
  for (const needle of [
    '<BPSActivityPanel plugin={plugin} />', 'api.getBPSActivity(plugin.id)', 'const BPS_ACTIVITY_REFRESH_MS = 2_500',
    'role="meter"', '<div className="data-table-shell">', "usePersistedPageSize('bps_activity', 20, DEFAULT_PAGE_SIZE_OPTIONS)",
    'h-1.5 overflow-hidden rounded-full bg-muted', "'bg-destructive'", 'animate-pulse', '<Pagination',
  ]) assert.ok(page.includes(needle), needle)
  const server = readFileSync(srcRoot + '../../admin/bps_dashboard.go', 'utf8')
  assert.ok(server.includes('return rows[i].InFlight > rows[j].InFlight'), 'server sorts busy accounts first')
})

test('zero timestamps are unset, not cooldowns', () => {
  const now = Date.parse('2026-09-29T10:00:00Z')
  assert.equal(hasTime('0001-01-01T00:00:00Z'), false, "Go's zero time")
  assert.equal(hasTime(''), false)
  assert.equal(hasTime(undefined), false)
  assert.equal(hasTime('2026-09-29T09:00:00Z'), true)
  assert.equal(activeUntil('0001-01-01T00:00:00Z', now), false)
  assert.equal(activeUntil('2026-09-29T09:59:59Z', now), false, 'a past cooldown is over')
  assert.equal(activeUntil('2026-09-29T10:05:00Z', now), true)
  const page = readPluginsUI()
  assert.ok(page.includes('const cooling = activeUntil(status?.cooling_until, now) ? status?.cooling_until : undefined'), 'the cooling line needs a running cooldown')
  assert.ok(page.includes('hasTime(block.next_probe_at)'), 'next probe guarded')
  assert.ok(page.includes('hasTime(block.blocked_at) ? formatBlockDuration('), 'elapsed guarded')
  const server = readFileSync(srcRoot + '../../proxy/bps_account_state.go', 'utf8')
  assert.ok(server.includes('`json:"cooling_until,omitzero"`'), 'the server omits unset cooling times')
})

test('disabled and invalid-credential accounts are not usable BPS accounts', () => {
  const page = readPluginsUI()
  assert.ok(page.includes("if (account.enabled === false) return t('plugins.accountDisabled')"), 'the account list labels disabled accounts')
  assert.ok(page.includes("if (account.codex_bps_credential_invalid) return t('plugins.accountInvalid')"), 'and invalid-credential ones')
  assert.ok(page.includes('{account.enabled !== false && !account.codex_bps_credential_invalid && <PluginAccountStatusLine'), 'and shows them no BPS status')
  assert.ok(page.includes("t('plugins.dashDisabled')") && page.includes("t('plugins.dashInvalid')"), 'the hero counts them apart from usable')
  const server = readFileSync(srcRoot + '../../admin/bps_dashboard.go', 'utf8')
  assert.ok(server.includes('case !account.IsEnabled():') && server.includes('case account.CredentialInvalid():'), 'the dashboard pool skips disabled and invalid accounts')
  assert.ok(readFileSync(srcRoot + '../../admin/codex_bps_account.go', 'utf8').includes('view.Eligible && row.Enabled &&'), 'codex_bps_active needs an enabled account')
})

test('BPS traffic charts: full bucket grid, health strip, shared chart theme', () => {
  const now = Date.parse('2026-09-29T10:30:20Z')
  const hour = bpsTrafficSeries([
    { bucket: '2026-09-29T10:30:00Z', requests: 5, succeeded: 3, errors_4xx: 1, errors_5xx: 0, org_rate_limited: 1, rate_limited: 0, policy_blocked: 1 },
    { bucket: '2026-09-29T10:00:00Z', requests: 2, succeeded: 2, errors_4xx: 0, errors_5xx: 0, org_rate_limited: 0, rate_limited: 0, policy_blocked: 0 },
    { bucket: '2026-09-29T08:00:00Z', requests: 9, succeeded: 9, errors_4xx: 0, errors_5xx: 0, org_rate_limited: 0, rate_limited: 0, policy_blocked: 0 },
  ], '1h', now)
  assert.equal(hour.length, 60, '60 one-minute buckets')
  assert.equal(hour[59].bucket, '2026-09-29T10:30:00.000Z', 'ends at the current bucket')
  assert.equal(hour[59].requests, 5)
  assert.equal(hour[59].policyBlocked, 1)
  assert.equal(hour[29].requests, 2)
  assert.equal(hour.reduce((sum, point) => sum + point.requests, 0), 7, 'buckets outside the range are dropped')
  const day = bpsTrafficSeries([], '24h', now)
  assert.equal(day.length, 48, '48 half-hour buckets')
  assert.ok(day.every((point) => point.requests === 0), 'empty buckets are zero')
  const [strip] = bpsHealthTimeline([{ bucket: 'b', requests: 10, succeeded: 6, errors_4xx: 2, errors_5xx: 1, org_rate_limited: 0, rate_limited: 0, policy_blocked: 0 }])
  assert.equal(strip.requests - strip.errors_4xx - strip.errors_5xx, 6, 'the strip success matches Succeeded')
  const page = readPluginsUI()
  for (const needle of [
    '<ComposedChart data={series} margin={chartMargin}>', 'fill="url(#bps-request-gradient)"', '<SystemHealthBar chartData={healthData} timeRange={range}',
    '<SegmentedPillGroup', 'riskPalette(risk)', '<StatCard', 'stroke="hsl(var(--success))"', 'stroke="hsl(var(--warning))"', 'stroke="hsl(var(--info))"',
  ]) assert.ok(page.includes(needle), needle)
  assert.ok(read('components/DashboardUsageCharts.tsx').includes("from '../lib/chartTheme'"), 'dashboard charts share the theme constants')
  assert.ok(read('components/PoolRunwayCard.tsx').includes("import { riskPalette } from '../lib/riskPalette'"), 'the runway card shares the palette')
})

test('dual-route breakers and the pelican judge: config, route states, probes and samples', () => {
  const protection = bpsConfigGroups.find((group) => group.key === 'protection').fields
  for (const key of ['degrade_breaker_enabled', 'degrade_score_threshold', 'degrade_probe_model', 'degrade_probe_interval', 'degrade_probe_max_concurrent', 'degrade_cooldown_ladder']) {
    assert.ok(protection.includes(key), `${key} is in the 账号保护 card`)
    assert.ok(bpsConfigFields.some((field) => field.key === key), key)
  }
  assert.equal(bpsConfigFields.find((field) => field.key === 'degrade_score_threshold').defaultValue, 187)
  assert.equal(bpsConfigFields.find((field) => field.key === 'degrade_probe_max_concurrent').defaultValue, 2)
  assert.ok(!bpsConfigFields.some((field) => field.key.startsWith('native_degrade')), 'the mismatch-count fields are gone')
  assert.match(degradeVerdictClass('ok'), /emerald/)
  assert.match(degradeVerdictClass('degraded'), /red/)
  assert.match(degradeVerdictClass('bogus'), /slate/)
  const page = readPluginsUI()
  for (const needle of [
    '<NativeRouteLine route={account.native_route} now={now} />', "if (trigger === 'pelican') return t('plugins.nativeTriggerPelican'", "if (trigger === 'native_403') return '403'",
    '<RouteBadge route={block.route} />', '<RouteBadge route={total.route} />', "t('plugins.dashNativeDegraded')", "t('plugins.dashBPSDegraded')",
    '<DegradeLine account={account} onProbe={() => setProbeTarget(account)} />', "{plugin.id === 'bps' && <DegradeProbesPanel plugin={plugin} />}",
    'api.startDegradeProbes(plugin.id, [account.account_id], route)', "t('plugins.degradeCalibration'",
    'sandbox=""', 'srcDoc={probe.html}',
  ]) assert.ok(page.includes(needle), needle)
  assert.ok(!/sandbox="[^"]*allow-scripts/.test(page), 'samples never run scripts')
  const server = readFileSync(srcRoot + '../../proxy/bps_plugin.go', 'utf8')
  assert.ok(server.includes('`json:"degrade_score_threshold,omitempty"`') && server.includes('`json:"degrade_probe_max_concurrent,omitempty"`'), 'the server knows the fields')
  for (const name of ['zh', 'en', 'zh-TW']) {
    const locale = JSON.parse(read(`locales/${name}.json`))
    for (const key of ['dashNativeDegraded', 'dashBPSDegraded', 'nativeTriggerPelican', 'nativeRouteOk', 'nativeRouteOpen', 'routeBps', 'routeNative', 'policyRoute', 'degradeProbe', 'degradeCalibration', 'degradeViewSample', 'degradeHistory', 'degradeHistoryDesc', 'bpsDegradedDetail']) {
      assert.equal(typeof locale.plugins[key], 'string', `${name} ${key}`)
    }
    for (const verdict of ['ok', 'degraded', 'invalid']) assert.equal(typeof locale.plugins.degradeVerdicts[verdict], 'string')
    for (const trigger of ['manual', 'mismatch', 'scheduled', 'recovery']) assert.equal(typeof locale.plugins.degradeTriggers[trigger], 'string')
    assert.equal(typeof locale.plugins.dashStates.bps_degraded, 'string')
  }
})

test('dual_route_preference is a one-line switch in the config form', () => {
  const field = bpsConfigFields.find((item) => item.key === 'dual_route_preference')
  assert.equal(field.kind, 'choice')
  assert.deepEqual(field.options, ['native', 'bps'])
  assert.equal(field.defaultValue, 'native')
  assert.ok(bpsConfigGroups.find((group) => group.key === 'protection').fields.includes('dual_route_preference'))
  assert.ok(readPluginsUI().includes("case 'choice':"), 'rendered with the shared Select')
  const server = readFileSync(srcRoot + '../../proxy/bps_plugin.go', 'utf8')
  assert.ok(server.includes('return pinned || !native || currentBPSConfig().PrefersBPS()'))
  for (const name of ['zh', 'en', 'zh-TW']) {
    const locale = JSON.parse(read(`locales/${name}.json`))
    for (const value of ['native', 'bps']) assert.equal(typeof locale.plugins.bpsConfigChoices.dual_route_preference[value], 'string')
  }
})

test('upstream Basispoints settings are views of the BPS plugin', () => {
  const settings = read('pages/Settings.tsx')
  assert.ok(settings.includes('<Link to="/plugins/bps/overview"'), 'the Basispoints card points at the plugin')
  assert.ok(!settings.includes('codex_basispoints_'), 'Settings edits no Basispoints field; the plugin owns them')
  assert.ok(read('lib/settingsPayload.ts').includes('CODEX_BASISPOINTS_PREFIX'), 'a settings save never writes the plugin views back')
  const server = readFileSync(srcRoot + '../../admin/bps_upstream_takeover.go', 'utf8')
  assert.ok(server.includes('func translateExcelBPSMode') && server.includes('func saveBPSPluginGlobalEnabled'))
  assert.ok(server.includes('func saveBPSPluginCacheCreationAsInput') && server.includes('bpsCacheCreationConfigKey = "cache_creation_as_input"'), 'the cache-creation switch is the plugin config key')
  const field = bpsConfigFields.find((item) => item.key === 'cache_creation_as_input')
  assert.equal(field.kind, 'boolean')
  assert.equal(field.defaultValue, false)
  assert.ok(bpsConfigGroups.find((group) => group.key === 'billing').fields.includes('cache_creation_as_input'))
  assert.ok(readPluginsUI().includes('billing: <Receipt />'))
  const plugin = readFileSync(srcRoot + '../../proxy/bps_plugin.go', 'utf8')
  assert.ok(plugin.includes('`json:"cache_creation_as_input,omitempty"`'), 'the server knows the field')
  const gate = readFileSync(srcRoot + '../../proxy/openai_excel_bps_route.go', 'utf8')
  assert.ok(gate.includes('if !excelBPSAdapterEnabled.Load() {'), 'upstream adapter gate stays closed')
  for (const name of ['zh', 'en', 'zh-TW']) {
    const locale = JSON.parse(read(`locales/${name}.json`))
    for (const key of ['codexBasispointsPluginNote', 'codexBasispointsPluginLink']) assert.equal(typeof locale.settings[key], 'string', `${name} ${key}`)
    assert.equal(typeof locale.plugins.bpsConfig.cache_creation_as_input, 'string')
    assert.equal(typeof locale.plugins.bpsConfigHints.cache_creation_as_input, 'string')
    for (const part of ['title', 'description']) assert.equal(typeof locale.plugins.bpsConfigGroups.billing[part], 'string')
  }
})

test('one BPS UI per concern', () => {
  const exists = (path) => { try { read(path); return true } catch { return false } }
  for (const gone of ['components/ExcelBpsBadge.tsx', 'lib/excelBpsStatus.ts', 'lib/excelBpsModels.ts']) assert.ok(!exists(gone), `${gone} is deleted`)
  const accounts = read('pages/Accounts.tsx')
  assert.ok(accounts.includes('<BPSAccountStatusProvider accounts={pagedAccounts}>'), 'Accounts loads the plugin account state')
  assert.ok(accounts.includes('<BPSStatusBadge accountId={account.id} active={account.codex_bps_active} />'), 'row: the BPS pill, from codex_bps_active')
  assert.ok(accounts.includes('<BPSStatusBadge accountId={account.id} active={account.codex_bps_active} variant="card" />'), 'card: the same pill')
  assert.equal(accounts.split('<BPSStatusBadge ').length - 1, 2, 'one BPS indicator per row and per card')
  assert.ok(!accounts.includes('account.codex_bps_active && <span') && !accounts.includes('<BPSAccountStatus '), 'no second BPS pill or status line')
  // Row: the pill sits right after the status badge, above the health bar.
  const row = accounts.slice(accounts.indexOf('errorMessage={account.error_message}'))
  assert.ok(row.indexOf('<BPSStatusBadge') < row.indexOf('<UsingCreditsBadge') && row.indexOf('<UsingCreditsBadge') < row.indexOf('<AccountHealthBar'), 'next to the status, above the health bar')
  assert.ok(read('pages/Plugins.tsx').includes("import { PluginAccountStatusLine } from '../components/PluginAccountStatusLine'"), 'Plugins and Accounts share the status line')
  const types = read('types.ts')
  for (const field of ['openai_excel_bps', 'bps_pause', 'ExcelBpsPauseView']) assert.ok(!types.includes(field), `${field} is gone from the client types`)
  const sheet = read('components/AccountQuickConfigSheet.tsx')
  assert.ok(!sheet.includes('excelBps') && !sheet.includes('clearAccountExcelBpsPause'), 'quick config has one BPS control')
  assert.ok(sheet.includes('<BPSAccountFields compact'), 'quick config keeps the plugin tri-state')
  assert.ok(read('lib/accountQuickConfig.ts').includes('form.bps.enabled !== form.initialBPSEnabled'), 'the switch is sent only when it changed')
  const badges = read('lib/errorBadges.ts')
  assert.ok(!badges.includes('basispoints_upstream_error') && !badges.includes('basispoints_transport_error'), 'no adapter-only error kinds')
  assert.ok(badges.includes('basispoints_cutoff_completed') && badges.includes('basispoints_model_access_changed'))
  const plugins = readFileSync(srcRoot + '../../admin/transport_plugins.go', 'utf8')
  const scheduler = readFileSync(srcRoot + '../../admin/codex_bps_account.go', 'utf8')
  const compat = readFileSync(srcRoot + '../../admin/bps_upstream_takeover.go', 'utf8')
  for (const [name, source] of [['Plugins-page switch', plugins], ['scheduler/batch fields', scheduler], ['upstream compat', compat]]) {
    assert.ok(source.includes('putBPSOverride('), `${name} writes the switch through putBPSOverride`)
  }
  for (const name of ['zh', 'en', 'zh-TW']) {
    const locale = JSON.parse(read(`locales/${name}.json`))
    assert.ok(!Object.keys(locale.accounts).some((key) => key.startsWith('excelBps')), `${name}: no Excel BPS account strings`)
  }
})

test('the Accounts BPS pill is toned by the plugin state', () => {
  const now = Date.parse('2026-10-01T08:00:00Z')
  const later = '2026-10-01T09:30:00Z'
  const past = '2026-10-01T07:00:00Z'
  assert.deepEqual(bpsBadgeView(undefined, now), { tone: 'active' })
  assert.deepEqual(bpsBadgeView({ account_id: 1, budget: 150, budget_used: 3 }, now), { tone: 'active' })
  assert.deepEqual(bpsBadgeView({ account_id: 1, cooling_until: later, reason: 'bps_rate_limited' }, now), { tone: 'cooling', cause: 'cooldown', until: later })
  assert.deepEqual(bpsBadgeView({ account_id: 1, cooling_until: past, reason: 'bps_rate_limited' }, now), { tone: 'active' }, 'an elapsed cooldown is normal')
  assert.deepEqual(bpsBadgeView({ account_id: 1, budget: 150, budget_used: 150 }, now), { tone: 'cooling', cause: 'budget' })
  assert.deepEqual(bpsBadgeView({ account_id: 1, cooling_until: later, reason: 'bps_policy_blocked', policy_tier: 2, policy_tiers: 4 }, now), { tone: 'blocked', until: later, tier: 2, tiers: 4 })
  assert.equal(bpsBadgeView({ account_id: 1, probe_pending: true, policy_tier: 1, policy_tiers: 4 }, now).tone, 'blocked', 'still blocked until its probe clears it')
  assert.deepEqual(bpsBadgeView({ account_id: 1, bps_degraded: true, bps_degraded_until: later }, now), { tone: 'degraded', until: later })
  const badge = read('components/PluginAccountStatusLine.tsx')
  for (const needle of ['<Layers ', 'BPS_BADGE_ICONS[view.tone]', '<TooltipContent', '<PluginAccountStatusLine status={status}', 'bg-emerald-50 text-emerald-700', 'bg-amber-50 text-amber-700', 'bg-rose-50 text-rose-700']) {
    assert.ok(badge.includes(needle.replace('<Layers ', 'Layers')), needle)
  }
  for (const name of ['zh', 'en', 'zh-TW']) {
    const locale = JSON.parse(read(`locales/${name}.json`))
    for (const key of ['active', 'cooling', 'coolingUntil', 'budget', 'blocked', 'blockedTier', 'degraded', 'title']) {
      assert.equal(typeof locale.accounts.bpsBadge[key], 'string', `${name} ${key}`)
    }
  }
})
