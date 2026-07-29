import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const pageSource = readFileSync(new URL('../pages/PromptFilter.tsx', import.meta.url), 'utf8')
const apiSource = readFileSync(new URL('../api.ts', import.meta.url), 'utf8')
const handlerSource = readFileSync(new URL('../../../admin/handler.go', import.meta.url), 'utf8')

test('prompt intelligence uses the persistent candidate lifecycle API', () => {
  for (const fragment of [
    '/prompt-filter/intelligence/candidates?',
    '/prompt-filter/intelligence/candidates/${id}/evidence',
    '/prompt-filter/intelligence/candidates/${id}/publish',
    '/prompt-filter/intelligence/candidates/${id}/dismiss',
  ]) {
    assert.equal(apiSource.includes(fragment), true, `missing candidate API: ${fragment}`)
  }
  assert.equal(apiSource.includes('addPromptIntelligenceRule'), false)
  assert.equal(apiSource.includes("'/prompt-filter/intelligence/rules'"), false)
  assert.equal(handlerSource.includes('POST("/prompt-filter/intelligence/rules"'), false)
})

test('pending evidence cannot be published and every lifecycle action stays in the review queue', () => {
  assert.equal(pageSource.includes("candidate.lifecycle_status === 'pending' && candidate.kind === 'pattern'"), true)
  assert.equal(pageSource.includes("candidate.lifecycle_status === 'pending' ?"), true)
  assert.equal(pageSource.includes('getPromptIntelligenceCandidateEvidence(candidate.id)'), true)
  assert.equal(pageSource.includes('publishPromptIntelligenceCandidate(candidate.id)'), true)
  assert.equal(pageSource.includes('dismissPromptIntelligenceCandidate(dismissTarget.id)'), true)
  assert.equal(pageSource.includes('result.staged'), true)
})

test('legacy automatic rule admission is not exposed as a setting', () => {
  assert.equal(pageSource.includes("t('promptFilter.intelligence.autoAdd')"), false)
  assert.equal(pageSource.includes('config.intelligence.auto_add'), false)
  assert.equal(pageSource.includes("setBool('intelligence', 'auto_add'"), false)
  assert.equal(pageSource.includes("{ path: ['intelligence', 'auto_add'], remove: true }"), true, 'legacy data should be cleaned when advanced settings are next saved')
})
