import type { PluginAccountStatus } from '../types'
import { activeUntil } from './transportPlugins.ts'

// The one BPS indicator of an Accounts row / card (upstream's "BPS" pill next
// to the status badge), driven by the plugin's account state:
//   active   green  BPS serves the account normally
//   cooling  amber  rate-limit cooldown (until) or request budget used up
//   blocked  red    usage-policy block (tier n/N), until its probe clears it
//   degraded red    the degradation breaker broke the BPS route
export type BPSBadgeTone = 'active' | 'cooling' | 'blocked' | 'degraded'

export interface BPSBadgeView {
  tone: BPSBadgeTone
  /** Why the pill is amber: a timed cooldown or the request budget. */
  cause?: 'cooldown' | 'budget'
  /** End of the cooldown / block / breaker, when known and still ahead. */
  until?: string
  tier?: number
  tiers?: number
}

export function bpsBadgeView(status: PluginAccountStatus | undefined, nowMs: number): BPSBadgeView {
  if (!status) return { tone: 'active' }
  const cooling = activeUntil(status.cooling_until, nowMs) ? status.cooling_until : undefined
  if ((cooling && status.reason === 'bps_policy_blocked') || status.probe_pending) {
    return { tone: 'blocked', until: cooling, tier: status.policy_tier || undefined, tiers: status.policy_tiers || undefined }
  }
  if (status.bps_degraded) {
    return { tone: 'degraded', until: activeUntil(status.bps_degraded_until, nowMs) ? status.bps_degraded_until : undefined }
  }
  if (cooling) return { tone: 'cooling', cause: 'cooldown', until: cooling }
  if (status.budget && (status.budget_used ?? 0) >= status.budget) return { tone: 'cooling', cause: 'budget' }
  return { tone: 'active' }
}
