import type { ModelPricingOverride } from '../types.ts'

export type PricingPreviewRate = {
  input: number
  cached: number
  output: number
}

export type ModelPricingPreview = {
  mode: 'single' | 'tiered'
  threshold: number
  standard: PricingPreviewRate
  long: PricingPreviewRate | null
  priority: PricingPreviewRate | null
  longPriority: PricingPreviewRate | null
  flexMultiplier: number
  expression: string
}

function hasRate(value: PricingPreviewRate | null): value is PricingPreviewRate {
  return Boolean(value && (value.input > 0 || value.cached > 0 || value.output > 0))
}

function multiplyRate(value: PricingPreviewRate, multiplier: number): PricingPreviewRate {
  return { input: value.input * multiplier, cached: value.cached * multiplier, output: value.output * multiplier }
}

function numberValue(value: unknown): number {
  const parsed = typeof value === 'number' ? value : Number(value)
  return Number.isFinite(parsed) ? parsed : 0
}

function rate(input: unknown, cached: unknown, output: unknown): PricingPreviewRate {
  return {
    input: numberValue(input),
    cached: numberValue(cached),
    output: numberValue(output),
  }
}

function rateExpression(value: PricingPreviewRate): string {
  return `p * ${value.input} + c * ${value.output} + cr * ${value.cached}`
}

export function buildModelPricingPreview(
  pricing: ModelPricingOverride = {},
): ModelPricingPreview {
  const standard = rate(pricing.input, pricing.cached_input, pricing.output)
  const threshold = Math.max(
    0,
    Math.round(numberValue(pricing.long_context_threshold_tokens)),
  )
  const candidateLong = rate(
    pricing.input_long,
    pricing.cached_input_long,
    pricing.output_long,
  )
  const long = threshold > 0 && hasRate(candidateLong) ? candidateLong : null
  const candidatePriority = rate(
    pricing.input_priority,
    pricing.cached_input_priority,
    pricing.output_priority,
  )
  const priority = hasRate(candidatePriority) ? candidatePriority : null
  const candidateLongPriority = rate(
    pricing.input_long_priority,
    pricing.cached_input_long_priority,
    pricing.output_long_priority,
  )
  const longPriority = long && hasRate(candidateLongPriority)
    ? candidateLongPriority
    : null
  const mode = long ? 'tiered' : 'single'
  const baseExpression = long
    ? `len < ${threshold} ? tier("standard", ${rateExpression(standard)}) : tier("long_context", ${rateExpression(long)})`
    : `tier("standard", ${rateExpression(standard)})`
  const priorityExpression = priority
    ? long && longPriority
      ? `len < ${threshold} ? tier("priority", ${rateExpression(priority)}) : tier("long_priority", ${rateExpression(longPriority)})`
      : `tier("priority", ${rateExpression(priority)})`
    : `${baseExpression} * 2`
  const expression = `param("service_tier") == "flex" ? (${baseExpression}) * 0.5 : (param("service_tier") == "priority" || param("service_tier") == "fast" ? ${priorityExpression} : ${baseExpression})`

  return {
    mode,
    threshold,
    standard,
    long,
    priority,
    longPriority,
    flexMultiplier: 0.5,
    expression,
  }
}

export function resolveModelPricingPreviewPriority(preview: ModelPricingPreview) {
  const priority = preview.priority ?? (hasRate(preview.standard) ? multiplyRate(preview.standard, 2) : null)
  const longPriority = preview.long
    ? preview.longPriority ?? (hasRate(preview.long) ? multiplyRate(preview.long, 2) : null)
    : null
  return { priority, longPriority }
}
