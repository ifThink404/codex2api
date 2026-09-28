import type { BuiltinPromptRuleFields, PromptFilterRule } from '../types'

export const promptRuleConditionKeys = ['all_patterns', 'any_patterns', 'exclude_patterns', 'authorization_exclude_patterns'] as const
export type PromptRuleConditionKey = typeof promptRuleConditionKeys[number]

export const defaultPromptRuleFilters = { name: '', category: '', participation: '', strength: '', modification: '', enabled: '', source: '' }
export type PromptRuleFilters = typeof defaultPromptRuleFilters

export function matchesPromptRuleFilters(rule: PromptFilterRule, filters: PromptRuleFilters): boolean {
  const query = filters.name.trim().toLowerCase()
  if (query && !rule.name.toLowerCase().includes(query)) return false
  if (filters.category && rule.category !== filters.category) return false
  if (filters.source && Boolean(rule.builtin) !== (filters.source === 'builtin')) return false
  if (filters.participation && promptRuleParticipates(rule) !== (filters.participation === 'execution')) return false
  if (filters.strength && Boolean(rule.strict) !== (filters.strength === 'strict')) return false
  if (filters.enabled && (rule.enabled !== false) !== (filters.enabled === 'enabled')) return false
  // Custom rules have no release baseline; they are neither modified nor default built-ins.
  if (filters.modification && (!rule.builtin || Boolean(rule.overridden) !== (filters.modification === 'modified'))) return false
  return true
}

export function promptRuleParticipates(rule: Pick<PromptFilterRule, 'signal_only' | 'strict'>): boolean {
  return !rule.signal_only || Boolean(rule.strict)
}

export function promptRuleFields(rule: PromptFilterRule): BuiltinPromptRuleFields {
  return {
    name: rule.name, pattern: rule.pattern, weight: rule.weight, category: rule.category || '', strict: Boolean(rule.strict),
    signal_only: Boolean(rule.signal_only), min_matches: rule.min_matches ?? 0,
    all_patterns: [...(rule.all_patterns ?? [])], any_patterns: [...(rule.any_patterns ?? [])],
    exclude_patterns: [...(rule.exclude_patterns ?? [])], authorization_exclude_patterns: [...(rule.authorization_exclude_patterns ?? [])],
  }
}

export function promptRuleConditionsValid(rule: Pick<PromptFilterRule, 'pattern' | PromptRuleConditionKey>, minimum: string): boolean {
  if (!/^\d+$/.test(minimum.trim())) return false
  const count = Number(minimum)
  if (!Number.isSafeInteger(count) || count > (rule.any_patterns?.length ?? 0)) return false
  if (!rule.pattern.trim() && !rule.all_patterns?.length && !rule.any_patterns?.length) return false
  return promptRuleConditionKeys.every(key => (rule[key] ?? []).length <= 128 && (rule[key] ?? []).every(pattern => Boolean(pattern.trim())))
}
