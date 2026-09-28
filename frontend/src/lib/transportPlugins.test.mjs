import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import { fileURLToPath } from 'node:url'
import { PLUGIN_VIEWS, bpsConfigFields, captureIdFromEvidence, pluginCaptureAgentFilters, pluginCaptureSource, normalizePluginView, parsePluginConfigText, pluginMetaSummary, sampleRateFromPercent, sampleRateToPercent } from './transportPlugins.ts'

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
  for (const dir of ['request', 'response', 'error']) used.add(`directions.${dir}`)
  for (const field of bpsConfigFields) used.add(`bpsConfig.${field.key}`)
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
