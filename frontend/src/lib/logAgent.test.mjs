import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { compactFilters, formatConfidence, logAgentLanguage, opsErrorLogAgentFilters, runFindings, usageLogIdFromEvidence } from './logAgent.ts'

const read = (p) => readFileSync(new URL(p, import.meta.url), 'utf8')
const locales = ['zh', 'en', 'zh-TW'].map((name) => JSON.parse(read(`../locales/${name}.json`)))

test('runFindings tolerates failed runs and partial findings', () => {
  assert.equal(runFindings(null), null)
  assert.equal(runFindings({ findings: {} }), null)
  assert.equal(runFindings({ findings: { summary: '' } }), null)
  assert.equal(runFindings({ findings: { summary: '', fallback: true } })?.fallback, true)
  assert.deepEqual(runFindings({ findings: { summary: 's', confidence: 0.4 } }), {
    summary: 's', root_causes: [], suggested_actions: [], confidence: 0.4, fallback: false,
  })
})

test('formatting and mapping helpers', () => {
  assert.equal(formatConfidence(0.756), '76%')
  assert.equal(formatConfidence(0), '-')
  assert.equal(formatConfidence(3), '100%')
  assert.equal(logAgentLanguage('zh-CN'), 'zh')
  assert.equal(logAgentLanguage('zh-TW'), 'zh-TW')
  assert.equal(logAgentLanguage('en-US'), 'en')
  assert.deepEqual(compactFilters({ a: ' x ', b: '', c: undefined }), { a: 'x' })
  assert.deepEqual(
    opsErrorLogAgentFilters({ status: '5xx', errorKind: '', endpoint: '/v1/responses', apiKeyId: '3', stream: 'true', q: ' boom ' }),
    { status: '5xx', endpoint: '/v1/responses', api_key_id: '3', stream: 'true', q: 'boom' },
  )
  assert.equal(usageLogIdFromEvidence('usage:42'), 42)
  assert.equal(usageLogIdFromEvidence('cap:42'), null)
})

test('LogAgentPanel is reusable and uses shared controls only', () => {
  const panel = read('../components/LogAgentPanel.tsx')
  assert.match(panel, /export interface LogAgentPanelProps/)
  for (const prop of ['source: string', 'refs\\?: string\\[\\]', 'filters\\?:', 'getRange\\?:', 'onEvidenceClick\\?:', 'bare\\?: boolean', 'showHistory\\?: boolean']) {
    assert.match(panel, new RegExp(prop), prop)
  }
  assert.match(panel, /api\.analyzeLogAgent\(/)
  assert.match(panel, /api\.listLogAgentRuns\(/)
  assert.match(panel, /api\.getLogAgentConfig\(\)/)
  assert.match(panel, /<Select\n/)
  assert.match(panel, /RefreshCw className="size-3\.5 animate-spin"/)
  assert.doesNotMatch(panel, /<select|<button|<input|type="checkbox"/)
  for (const key of new Set(panel.match(/'logAgent\.[A-Za-z.]+'/g).map((k) => k.slice(1, -1)))) {
    for (const locale of locales) {
      assert.equal(typeof key.split('.').reduce((node, part) => node?.[part], locale), 'string', key)
    }
  }
  for (const locale of locales) {
    for (const group of ['status', 'category', 'priority']) {
      assert.ok(Object.keys(locale.logAgent[group]).length >= 3, `logAgent.${group}`)
    }
    for (const category of ['upstream', 'account', 'gateway', 'network', 'client', 'config', 'unknown']) {
      assert.equal(typeof locale.logAgent.category[category], 'string', category)
    }
  }
})

test('operations errors page is the first consumer of the panel', () => {
  const page = read('../pages/OperationsErrors.tsx')
  assert.match(page, /<LogAgentPanel\n\s+source="ops_errors"\n\s+filters=\{opsErrorLogAgentFilters\(buildBaseParams\(\)\)\}/)
  assert.match(page, /source="usage_logs"\n\s+refs=\{\[selectedLog\.request_id\]\}/)
  for (const locale of locales) {
    for (const key of ['logAgentDesc', 'logAgentRequestTitle', 'logAgentRequestDesc']) {
      assert.equal(typeof locale.opsErrors[key], 'string', `opsErrors.${key}`)
    }
    assert.match(locale.logAgent.evidenceNotOnPage, /\{\{id\}\}/)
  }
})
