type UsageTierSource = {
  service_tier?: string | null
  requested_service_tier?: string | null
  actual_service_tier?: string | null
  billing_service_tier?: string | null
}

export function getUsageBillingTier(log: UsageTierSource): string {
  const billing = log.billing_service_tier?.trim()
  if (billing) return billing
  // Split-tier records use empty for base billing, even if upstream ran Fast.
  if (log.requested_service_tier?.trim() || log.actual_service_tier?.trim()) {
    return 'default'
  }
  // Preserve historical records that only had the single legacy tier field.
  return log.service_tier?.trim() || ''
}
