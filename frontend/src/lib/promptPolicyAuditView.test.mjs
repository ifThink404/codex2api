import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const source = readFileSync(new URL('../pages/PromptFilter.tsx', import.meta.url), 'utf8')
const types = readFileSync(new URL('../types.ts', import.meta.url), 'utf8')

test('CY incidents and local logs use independent pagination state', () => {
  assert.match(source, /usePersistedPageSize\('prompt_policy_incidents'/)
  assert.match(source, /page: incidentPage, pageSize: incidentPageSize/)
  assert.match(source, /page: logPage,/)
  assert.match(source, /page=\{incidentPage\}[\s\S]*totalItems=\{incidentTotal\}/)
  assert.match(source, /page=\{logPage\}[\s\S]*totalItems=\{total\}/)
  assert.doesNotMatch(source, /Math\.max\(total, incidentTotal\)/)
})

test('CY routing snapshots and NewAPI audit passthrough are visible', () => {
  for (const field of [
    'account_name',
    'account_group_names',
    'api_key_allowed_group_names',
    'local_comparison',
    'prompt_available',
    'newapi_policy_status',
    'newapi_request_id',
    'newapi_decision_id',
  ]) {
    assert.match(types, new RegExp(`${field}[?:]`))
  }
  assert.match(source, /cyberComparisonStatus/)
  assert.match(source, /newapiPolicyStatus/)
})
