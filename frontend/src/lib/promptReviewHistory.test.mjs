import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const source = readFileSync(new URL('../pages/PromptFilter.tsx', import.meta.url), 'utf8')
const api = readFileSync(new URL('../api.ts', import.meta.url), 'utf8')
const types = readFileSync(new URL('../types.ts', import.meta.url), 'utf8')
const zh = JSON.parse(readFileSync(new URL('../locales/zh.json', import.meta.url), 'utf8'))

test('model review history has independent pagination and excludes it from local audit rows', () => {
  assert.match(source, /usePersistedPageSize\('prompt_review_logs'/)
  assert.match(source, /page: reviewPage,[\s\S]*reviewed: true/)
  assert.match(source, /page: logPage,[\s\S]*reviewed: false/)
  assert.match(source, /<PromptReviewLogsTable logs=\{reviewLogs\}/)
  assert.match(source, /page=\{reviewPage\}[\s\S]*totalItems=\{reviewTotal\}/)
  assert.match(api, /typeof params\.reviewed === 'boolean'/)
})

test('model review history exposes parsed request and response metadata without secrets', () => {
  for (const field of [
    'reviewed',
    'review_confidence',
    'review_threshold',
    'review_reason',
    'review_endpoint',
    'review_request_mode',
    'review_latency_ms',
  ]) {
    assert.match(types, new RegExp(`${field}:`))
  }
  assert.match(source, /log\.text_preview/)
  assert.match(source, /log\.review_confidence/)
  assert.match(source, /log\.review_reason/)
  assert.doesNotMatch(source, /review_api_key.*PromptReviewLogsTable/)
  assert.match(zh.promptFilter.reviewHistoryDesc, /不保存审核 Key、Authorization 或原始 Payload/)
})
