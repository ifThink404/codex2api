import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import ts from 'typescript'

const read = (path) => readFileSync(new URL(path, import.meta.url), 'utf8')

test('usage audit renders one response-model line per layout and retains turn-state evidence', () => {
  const usage = read('../pages/Usage.tsx')
  assert.equal((usage.match(/<UsageResponseModel\b/g) ?? []).length, 2)
  assert.equal((usage.match(/<UsageTurnState\b/g) ?? []).length, 2)
  assert.equal((usage.match(/<UsageWindowNumberBadge\b/g) ?? []).length, 2)
  assert.doesNotMatch(usage, /<UpstreamResponseModelBadge\b/)
  const component = read('../components/UsageResponseModel.tsx')
  for (const key of ['responseModel', 'modelVariant', 'modelMismatch', 'modelMismatchFastTierHint']) {
    assert.ok(component.includes(`usage.${key}`), `shared renderer must retain ${key}`)
  }
  assert.match(component, /log\.upstream_model_mismatch/)
})

test('mismatch and all turn-state filters participate in the same live query callback', () => {
  const usage = read('../pages/Usage.tsx')
  assert.doesNotMatch(usage, /^(<<<<<<<|=======|>>>>>>>)/m)
  const body = usage.split('const buildDimensionFilterParams = useCallback(')[1]?.split('const buildLogFilterParams')[0] ?? ''
  const dependencies = body.slice(body.lastIndexOf('}, ['))
  for (const name of ['filterModelMismatch', 'filterTurnState', 'filterTurnStateEcho', 'filterTurnStateStripped']) {
    assert.ok(dependencies.includes(name), `${name} must invalidate the callback`)
  }
  for (const field of ['upstreamModelMismatch:', 'turnState:', 'turnStateEcho:', 'turnStateStripped:']) {
    assert.ok(body.includes(field), `query must retain ${field}`)
  }
})

test('proxy field combines custom issuance hint with existing matched proxy and timezone details', () => {
  const source = read('../components/ProxyField.tsx')
  assert.doesNotMatch(source, /^(<<<<<<<|=======|>>>>>>>)/m)
  assert.match(source, /linkedHint \?\? t\("accounts.proxyPoolLinkedHint"\)/)
  assert.match(source, /accounts.proxyPoolMatched/)
  assert.match(source, /accounts.proxyPoolUnmatched/)
  assert.match(source, /<ProxyTimezoneHint/)
})

test('merged locales keep probes and audit while removing retired template controls', () => {
  for (const locale of ['zh', 'en', 'zh-TW']) {
    const source = read(`../locales/${locale}.json`)
    assert.doesNotMatch(source, /^(<<<<<<<|=======|>>>>>>>)/m)
    const messages = JSON.parse(source)
    const ast = ts.parseJsonText(`${locale}.json`, source)
    function visit(node) {
      if (ts.isObjectLiteralExpression(node)) {
        const keys = node.properties.map((property) => property.name?.text)
        assert.equal(new Set(keys).size, keys.length, `${locale}: duplicate locale key`)
      }
      ts.forEachChild(node, visit)
    }
    visit(ast)
    assert.ok(messages.accounts.probePolicyTitle)
    assert.ok(messages.accounts.apiAutoRecoveryHint)
    assert.equal(messages.accounts.codexTurnStateProxyPoolHint, undefined)
    assert.equal(messages.accounts.turnStateStatus, undefined)
    assert.equal(messages.turnStateHistory, undefined)
    assert.ok(messages.usage.turnState)
    for (const key of ['responseModel', 'modelMismatch', 'modelVariant', 'filterModelMismatch', 'filterModelMismatchHint', 'modelMismatchFastTierHint', 'sentUpstreamModel', 'requestedModel']) {
      assert.ok(messages.usage[key], `${locale}: missing usage.${key}`)
    }
  }
})

test('UsageLog keeps a single response model field alongside mismatch and turn-state metadata', () => {
  const source = read('../types.ts').split('export interface UsageLog {')[1]?.split('\n}')[0] ?? ''
  assert.equal((source.match(/upstream_response_model\?:/g) ?? []).length, 1)
  for (const field of ['upstream_model_mismatch', 'turn_state_length', 'turn_state_echo', 'turn_state_stripped', 'window_number']) {
    assert.ok(source.includes(`${field}?:`))
  }
})
