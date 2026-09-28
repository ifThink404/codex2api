import assert from 'node:assert/strict'
import { test } from 'node:test'
import { defaultPromptRuleFilters, matchesPromptRuleFilters, promptRuleFields, promptRuleParticipates, promptRuleConditionsValid } from './promptRuleFields.ts'

test('participation matches engine semantics including legacy strict signal-only rules', () => {
  assert.equal(promptRuleParticipates({}), true)
  assert.equal(promptRuleParticipates({ signal_only: true }), false)
  assert.equal(promptRuleParticipates({ signal_only: true, strict: true }), true)
})

test('complete editing snapshot preserves every condition without sharing arrays', () => {
  const rule = { name: 'sample', pattern: '', weight: 70, strict: false, signal_only: true,
    all_patterns: ['one\ntwo'], any_patterns: ['alpha', 'beta'], min_matches: 2,
    exclude_patterns: ['ordinary sample'], authorization_exclude_patterns: ['test fixture'] }
  const fields = promptRuleFields(rule)
  for (const key of ['all_patterns', 'any_patterns', 'exclude_patterns', 'authorization_exclude_patterns']) {
    assert.deepEqual(fields[key], rule[key])
    assert.notEqual(fields[key], rule[key])
  }
  assert.equal(fields.signal_only, true)
  assert.equal(fields.min_matches, 2)
  assert.deepEqual(promptRuleFields({ name: 'old', pattern: 'old expression', weight: 50 }).all_patterns, [])
})

test('composite-only drafts are valid, empty and impossible conditions are rejected', () => {
  assert.equal(promptRuleConditionsValid({ pattern: '', all_patterns: ['required'] }, '0'), true)
  assert.equal(promptRuleConditionsValid({ pattern: '', any_patterns: ['a', 'b'] }, '2'), true)
  assert.equal(promptRuleConditionsValid({ pattern: '', exclude_patterns: ['excluded'] }, '0'), false)
  assert.equal(promptRuleConditionsValid({ pattern: 'main', all_patterns: [''] }, '0'), false)
  assert.equal(promptRuleConditionsValid({ pattern: 'main', authorization_exclude_patterns: ['  '] }, '0'), false)
  for (const minimum of ['-1', '1.2', '2', '', 'Infinity']) {
    assert.equal(promptRuleConditionsValid({ pattern: 'main', any_patterns: ['one'] }, minimum), false)
  }
})

test('name and label filters compose without confusing strict with audit-only', () => {
  const rules = [
    { name: 'Rule_Alpha', category: 'one', builtin: true, strict: true, signal_only: true, overridden: true, enabled: true },
    { name: 'rule_beta', category: 'one', builtin: true, signal_only: true, enabled: false },
    { name: 'custom_alpha', category: 'two', builtin: false, signal_only: false },
  ]
  const names = (filter) => rules.filter(rule => matchesPromptRuleFilters(rule, { ...defaultPromptRuleFilters, ...filter })).map(rule => rule.name)
  assert.deepEqual(names({ name: ' ALPHA ' }), ['Rule_Alpha', 'custom_alpha'])
  assert.deepEqual(names({ participation: 'execution' }), ['Rule_Alpha', 'custom_alpha'])
  assert.deepEqual(names({ participation: 'audit' }), ['rule_beta'])
  assert.deepEqual(names({ participation: 'execution', strength: 'strict', modification: 'modified', enabled: 'enabled', category: 'one' }), ['Rule_Alpha'])
  assert.deepEqual(names({ source: 'custom', participation: 'execution', strength: 'ordinary' }), ['custom_alpha'])
  assert.deepEqual(names({ modification: 'default' }), ['rule_beta'])
  assert.deepEqual(names({ modification: 'modified', source: 'custom' }), [])
  assert.deepEqual(names({ enabled: 'disabled' }), ['rule_beta'])
  assert.deepEqual(names({ name: 'does_not_exist' }), [])
  assert.equal(names({}).length, 3)
})

test('builtin rule editing is wired to the API, shared controls and three locales', async () => {
  const { readFileSync } = await import('node:fs')
  const page = readFileSync(new URL('../pages/PromptFilter.tsx', import.meta.url), 'utf8')
  const apiSource = readFileSync(new URL('../api.ts', import.meta.url), 'utf8')
  assert.match(apiSource, /updateBuiltinPromptRule: \(name: string, expected: BuiltinPromptRuleFields, rule: BuiltinPromptRuleFields \| null\)/)
  assert.match(apiSource, /\/prompt-filter\/rules\/builtin\/\$\{encodeURIComponent\(name\)\}/)
  assert.match(page, /api\.updateBuiltinPromptRule\(/)
  assert.doesNotMatch(page, /<select\b/)
  for (const locale of ['zh', 'en', 'zh-TW']) {
    const messages = JSON.parse(readFileSync(new URL(`../locales/${locale}.json`, import.meta.url), 'utf8')).promptFilter
    for (const key of ['editBuiltinRule', 'editBuiltinRuleDesc', 'restoreBuiltinRule', 'builtinModified', 'builtinSaved', 'builtinRestored', 'ruleExecution', 'ruleAuditOnly', 'ruleFiltersTitle']) {
      assert.ok(messages[key], `${locale}: promptFilter.${key}`)
    }
    assert.ok(messages.ruleConditions.authorization_exclude_patterns.label, `${locale}: ruleConditions`)
  }
})
