import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const source = readFileSync(new URL('../pages/PromptFilter.tsx', import.meta.url), 'utf8')
const api = readFileSync(new URL('../api.ts', import.meta.url), 'utf8')
const types = readFileSync(new URL('../types.ts', import.meta.url), 'utf8')
const zh = JSON.parse(readFileSync(new URL('../locales/zh.json', import.meta.url), 'utf8'))

test('risk profiles have independent list and event pagination', () => {
  assert.match(source, /usePersistedPageSize\('prompt_risk_profiles'/)
  assert.match(source, /usePersistedPageSize\('prompt_risk_profile_events'/)
  assert.match(api, /event_page=\$\{eventPage\}/)
  assert.match(source, /page=\{eventPage\}[\s\S]*totalItems=\{detail\?\.event_total \?\? 0\}/)
})

test('risk profile identity boundaries and operational guardrail are visible', () => {
  for (const subject of ['newapi_user', 'session', 'api_key', 'client_ip', 'upstream_account']) {
    assert.match(types, new RegExp(subject))
    assert.equal(typeof zh.promptFilter.risk.subjects[subject], 'string')
  }
  assert.match(source, /profile\.is_person/)
  assert.match(source, /identity_confidence/)
  assert.match(zh.promptFilter.risk.guardrail, /不会单独.*拦截/)
})

test('risk profile API supports filters and exact subject detail', () => {
  assert.match(api, /getPromptRiskProfiles/)
  assert.match(api, /subject_type/)
  assert.match(api, /risk_level/)
  assert.match(api, /min_score/)
  assert.match(api, /getPromptRiskProfile/)
  assert.match(api, /encodeURIComponent\(subjectType\)/)
  assert.match(api, /encodeURIComponent\(subjectKey\)/)
})
