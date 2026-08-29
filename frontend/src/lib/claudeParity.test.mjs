import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const channelFilter = readFileSync(new URL('../components/ChannelFilter.tsx', import.meta.url), 'utf8')
const dashboard = readFileSync(new URL('../pages/Dashboard.tsx', import.meta.url), 'utf8')
const usage = readFileSync(new URL('../pages/Usage.tsx', import.meta.url), 'utf8')
const apiKeys = readFileSync(new URL('../pages/APIKeys.tsx', import.meta.url), 'utf8')
const proxies = readFileSync(new URL('../pages/Proxies.tsx', import.meta.url), 'utf8')
const scheduler = readFileSync(new URL('../pages/SchedulerBoard.tsx', import.meta.url), 'utf8')
const claude = readFileSync(new URL('../pages/ClaudeAccounts.tsx', import.meta.url), 'utf8')
const types = readFileSync(new URL('../types.ts', import.meta.url), 'utf8')
const zh = JSON.parse(readFileSync(new URL('../locales/zh.json', import.meta.url), 'utf8'))

test('shared usage channel filter exposes Claude and persists it', () => {
  assert.match(channelFilter, /UsageChannel = "" \| "codex" \| "grok" \| "antigravity" \| "claude"/)
  assert.match(channelFilter, /raw === "claude"/)
  assert.match(channelFilter, /key: "claude"/)
  assert.match(channelFilter, /channel="claude"/)
})

test('dashboard renders Claude channel counters', () => {
  assert.match(dashboard, /'claude'/)
  assert.match(dashboard, /key === 'claude'/)
  assert.match(dashboard, /channel: key === 'claude' ? 'Claude'/)
})

test('usage and management filters keep Claude provider identity', () => {
  assert.match(usage, /log\.channel === 'claude'/)
  assert.match(usage, /channel === 'claude'/)
  assert.match(apiKeys, /claudeModelOptions/)
  assert.match(apiKeys, /key: "claude"/)
  assert.match(proxies, /BindKindFilter = "all" \| "codex" \| "grok" \| "claude"/)
  assert.match(proxies, /bindKindClaude/)
  assert.match(scheduler, /channel.*claude|claude.*channel/)
})

test('Claude rows expose sampling state and provider copy', () => {
  assert.match(claude, /usage_probe|usageProbe|sampled|unsampled/)
  assert.match(claude, /last.*sample|采样|sample/i)
  assert.match(types, /claude/)
  assert.equal(typeof zh.claude?.samplingState, 'object')
})
