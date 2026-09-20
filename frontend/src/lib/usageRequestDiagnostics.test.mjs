import assert from 'node:assert/strict'
import { test } from 'node:test'
import { compactionMetadataDiagnosticValue, diagnosticClientInfo, diagnosticEntries, diagnosticJSONDisplay, diagnosticOutboundIdentity, diagnosticRecord, diagnosticValueText, hasOmittedIncomingDiagnostic, splitOutboundIdentityDiagnostic, turnStateDiagnosticRows, usageRequestTypeLabelKey, usageRequestTypes } from './usageRequestDiagnostics.ts'

test('legacy omitted ingress is distinguished from absent or partially captured fields', () => {
  assert.equal(hasOmittedIncomingDiagnostic({ truncated: true, incoming: null }), true)
  for (const value of [null, undefined, {}, { incoming: null }, { truncated: true }, { truncated: true, incoming: {} }, { truncated: true, incoming: { headers: { 'Session-Id': 'original' } } }]) {
    assert.equal(hasOmittedIncomingDiagnostic(value), false)
  }
})

test('compaction metadata distinguishes absent, historical, malformed and separate carrier values', () => {
  for (const value of [undefined, null, {}, 'invalid']) {
    assert.deepEqual(compactionMetadataDiagnosticValue(value), { state: 'not_recorded', fields: {}, invalidFields: [] })
  }
  for (const state of ['absent', 'invalid_metadata', 'invalid_type', 'too_large']) {
    assert.equal(compactionMetadataDiagnosticValue({ state }).state, state)
  }
  const header = Object.freeze({ state: 'present', implementation: 'local', trigger: 'auto', extra: 'do not show' })
  const body = Object.freeze({ state: 'present', implementation: 'responses_compaction_v2', trigger: 42, invalid_fields: ['trigger', 'untrusted'] })
  assert.deepEqual(compactionMetadataDiagnosticValue(header), { state: 'present', fields: { implementation: 'local', trigger: 'auto' }, invalidFields: [] })
  assert.deepEqual(compactionMetadataDiagnosticValue(body), { state: 'present', fields: { implementation: 'responses_compaction_v2' }, invalidFields: ['trigger'] })
  assert.equal(body.trigger, 42)
})

test('turn-state comparison separates ingress, mapping, issued alias and actual wire values', () => {
  const alias = 'gAAAA' + 'A'.repeat(285) + '=='
  const real = 'gAAAA' + 'B'.repeat(285) + '=='
  const state = { events: [
    { action: 'restored', carrier: 'request_header', received: alias, alias, real, real_hash: 'hash:real' },
    { action: 'issued', carrier: 'response_metadata', alias, real },
  ] }
  const outbound = { http: { headers: { 'X-Codex-Turn-State': real } }, body: { client_metadata: { 'x-codex-turn-state': real } }, ws_handshake: { headers: { 'X-Codex-Turn-State': 'stale-handshake' } } }
  const rows = turnStateDiagnosticRows(state, outbound)
  assert.equal(rows.filter(row => row.kind === 'received').length, 1)
  assert.equal(rows.find(row => row.kind === 'received').value, alias)
  assert.equal(rows.find(row => row.kind === 'alias').value, alias)
  assert.equal(rows.filter(row => row.kind === 'upstream').length, 2)
  assert.ok(rows.filter(row => row.kind === 'upstream').every(row => row.value === real))
  assert.equal(rows.some(row => row.value === 'stale-handshake' || row.kind === 'hash'), false)
  assert.equal(turnStateDiagnosticRows(state, undefined).some(row => row.kind === 'upstream'), false)
  assert.equal(JSON.parse(JSON.stringify(rows)).find(row => row.kind === 'alias').value, alias)
})

test('turn-state historical and cleared values do not fabricate mapping or transmission', () => {
  assert.deepEqual(turnStateDiagnosticRows(undefined, undefined), [])
  assert.deepEqual(turnStateDiagnosticRows({ events: [{ action: 'cleared_unmanaged', carrier: 'request_header', real_hash: 'old-hash' }] }, null), [
    { kind: 'hash', value: 'old-hash', carrier: 'request_header', action: 'cleared_unmanaged' },
  ])
  const rows = turnStateDiagnosticRows({ events: [{ action: 'cleared_unmanaged', carrier: 'request_header', received: 'unmanaged', real_hash: 'hash' }] }, {})
  assert.deepEqual(rows.map(row => row.kind), ['received'])
})

test('outbound snapshots exclude gateway mapping and consistency diagnostics without mutating exports', () => {
  const mapping = Object.freeze({ changes: [{ original: 'original-turn', outbound: 'mapped-turn' }] })
  const source = Object.freeze({
    format_version: 2, session_consistency: 'matched', truncated: true, account_mapping: mapping, project_mapping: mapping,
    http: { headers: { 'Session-Id': 'session' } },
    ws_handshake: { headers: { 'X-Codex-Turn-Metadata': '{"turn_id":"mapped-turn"}' } },
    body: { client_metadata: { turn_id: 'mapped-turn' } },
    future_diagnostic: { local: true },
  })
  const original = JSON.stringify(source)
  const { snapshot, local } = splitOutboundIdentityDiagnostic(source)
  assert.deepEqual(Object.keys(snapshot), ['http', 'ws_handshake', 'body'])
  assert.equal(snapshot.body.client_metadata.turn_id, 'mapped-turn')
  assert.equal(snapshot.ws_handshake.headers['X-Codex-Turn-Metadata'], '{"turn_id":"mapped-turn"}')
  assert.equal(local.account_mapping, mapping)
  assert.equal(local.project_mapping, mapping)
  assert.equal(local.format_version, 2)
  assert.equal(local.session_consistency, 'matched')
  assert.equal(local.truncated, true)
  assert.deepEqual(local.future_diagnostic, { local: true })
  assert.equal(JSON.stringify(source), original)
})

test('outbound diagnostic split supports missing and historical captures', () => {
  for (const empty of [undefined, null, [], 'invalid']) {
    assert.deepEqual(splitOutboundIdentityDiagnostic(empty), { snapshot: {}, local: {} })
  }
  const historical = { body: { turn_metadata: { turn_id: 'old-turn' }, links: { prompt_cache_key: 'hash:old' } } }
  assert.deepEqual(splitOutboundIdentityDiagnostic(historical), { snapshot: historical, local: {} })
})

test('account mapping audit survives JSON display and copy without changing the input', () => {
  const mapping = { version: 'account-suffix-v1', status: 'mapped', changes: [{ original: 'original-id', outbound: 'account-id' }], cache_partitioned: true }
  const source = { format_version: 2, account_mapping: mapping, body: { client_metadata: { 'x-codex-turn-metadata': '{"session_id":"account-id"}' } } }
  const displayed = diagnosticJSONDisplay(source, true)
  assert.deepEqual(displayed.account_mapping, mapping)
  assert.deepEqual(JSON.parse(JSON.stringify(displayed)).account_mapping, mapping)
  assert.equal(source.body.client_metadata['x-codex-turn-metadata'], '{"session_id":"account-id"}')
})

test('JSON display preserves actual nesting and carrier types unless explicitly decoded', () => {
  const value = { format_version: 2, body: { prompt_cache_key: 'hash:cache', client_metadata: { 'x-codex-turn-metadata': '{"session_id":"session","window_number":2}' } } }
  assert.equal(diagnosticJSONDisplay(value), value)
  const decoded = diagnosticJSONDisplay(value, true)
  assert.deepEqual(decoded.body.client_metadata['x-codex-turn-metadata'], { session_id: 'session', window_number: 2 })
  assert.equal(typeof value.body.client_metadata['x-codex-turn-metadata'], 'string')
  assert.equal('links' in decoded.body, false)
  assert.equal('turn_metadata' in decoded.body, false)
  const old = { body: { turn_metadata: { session_id: 'old' }, links: { prompt_cache_key: 'hash:old' } } }
  assert.deepEqual(diagnosticJSONDisplay(old, true), old)
  assert.deepEqual(diagnosticJSONDisplay({ 'x-codex-turn-metadata': 'invalid' }, true), { 'x-codex-turn-metadata': 'invalid' })
})

test('historical and unknown request types are not inferred', () => {
  for (const value of usageRequestTypes) {
    assert.equal(usageRequestTypeLabelKey(value), `usage.diagnostics.types.${value}`)
  }
  assert.equal(usageRequestTypeLabelKey(), 'usage.diagnostics.types.not_recorded')
  assert.equal(usageRequestTypeLabelKey(''), 'usage.diagnostics.types.not_recorded')
  assert.equal(usageRequestTypeLabelKey('future'), 'usage.diagnostics.types.unknown')
  assert.equal(usageRequestTypeLabelKey('related_internal'), 'usage.diagnostics.types.related_internal')
})

test('diagnostic values distinguish missing from false and zero', () => {
  for (const value of [null, undefined, '', '0001-01-01T00:00:00Z', []]) assert.equal(diagnosticValueText(value), null)
  assert.equal(diagnosticValueText(false), 'false')
  assert.equal(diagnosticValueText(0), '0')
  assert.equal(diagnosticValueText(['concurrency', 'model']), 'concurrency, model')
})

test('diagnostic fields show missing source values and omit private state', () => {
  const entries = Object.fromEntries(diagnosticEntries({ thread_source: 'guardian_review', _internal: 'hidden', passive_authorized: false }, true))
  assert.equal(entries.thread_source, 'guardian_review')
  assert.equal(entries.request_kind, '')
  assert.equal(entries.passive_authorized, false)
  assert.equal('_internal' in entries, false)
  for (const value of [null, [], 'text', 17]) assert.deepEqual(diagnosticRecord(value), {})
})

test('device diagnostics retain distinct sources without guessing a winning installation ID', () => {
  const info = diagnosticClientInfo({
    headers: { 'X-Codex-Installation-Id': 'header-device', 'User-Agent': 'codex-tui/0.1', Authorization: 'secret' },
    turn_metadata_header: { installation_id: 'metadata-device', client_version: '0.1', prompt: 'private' },
    signed_newapi: { installation_id: 'signed-device', token_id: '12' },
    'metadata.user_id': { device_id: 'hash:claude-device', session_id: 'session' },
    _private: { installation_id: 'hidden' },
  })
  assert.equal(info['headers.X-Codex-Installation-Id'], 'header-device')
  assert.equal(info['turn_metadata_header.installation_id'], 'metadata-device')
  assert.equal(info['signed_newapi.installation_id'], 'signed-device')
  assert.equal(info['metadata.user_id.device_id'], 'hash:claude-device')
  assert.equal(info['headers.User-Agent'], 'codex-tui/0.1')
  assert.equal(Object.keys(info).some(key => key.includes('Authorization') || key.includes('prompt') || key.startsWith('_')), false)
  assert.equal(Object.hasOwn(info, 'installation_id'), false)
})

test('old diagnostic snapshots do not synthesize device information', () => {
  assert.deepEqual(diagnosticClientInfo(undefined), {})
  assert.deepEqual(diagnosticClientInfo({ signed_newapi: { root_fingerprint: 'root' } }), {})
  const fields = Object.fromEntries(diagnosticEntries({ window_number: 0 }, true))
  assert.equal(fields.window_number, 0)
  assert.equal(diagnosticValueText(fields.installation_id), null)
})

test('outbound identity separates actual handshake values from current frame values', () => {
  const info = diagnosticOutboundIdentity({
    http: { headers: { 'User-Agent': 'http-client' }, turn_metadata: { installation_id: 'http-device' } },
    ws_handshake: { headers: { 'User-Agent': 'original-handshake-client' }, turn_metadata: { window_id: 'thread:1' } },
    body: { client_metadata: { 'x-codex-installation-id': 'frame-device' }, turn_metadata: { window_id: 'thread:2' }, links: { previous_response_id: 'hash:response' }, input: 'not displayed' },
    incoming: { installation_id: 'not displayed' },
    _private: 'hidden',
  })
  assert.equal(info['http.headers.User-Agent'], 'http-client')
  assert.equal(info['ws_handshake.headers.User-Agent'], 'original-handshake-client')
  assert.equal(info['ws_handshake.turn_metadata.window_id'], 'thread:1')
  assert.equal(info['body.turn_metadata.window_id'], 'thread:2')
  assert.equal(info['body.client_metadata.x-codex-installation-id'], 'frame-device')
  assert.equal(info['body.links.previous_response_id'], 'hash:response')
  assert.equal(JSON.stringify(info).includes('not displayed'), false)
  assert.deepEqual(diagnosticOutboundIdentity(undefined), {})
  assert.deepEqual(diagnosticOutboundIdentity({ truncated: true }), { capture_truncated: true })
})

test('outbound session consistency displays recorded results without inferring historical values', () => {
  for (const result of ['matched', 'mismatched', 'missing_header', 'missing_body']) {
    assert.deepEqual(diagnosticOutboundIdentity({ session_consistency: result }), { session_consistency: result })
  }
  assert.deepEqual(diagnosticOutboundIdentity({ session_consistency: 'private-value' }), {})
  assert.deepEqual(diagnosticOutboundIdentity({ session_consistency: null }), {})
  assert.deepEqual(diagnosticOutboundIdentity({}), {})
})
