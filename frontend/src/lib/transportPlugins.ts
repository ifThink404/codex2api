// Transport plugin page helpers (pure, unit-tested).
import type { BPSTrafficPoint } from '../types'

export const PLUGIN_VIEWS = ['overview', 'captures', 'logs', 'errors', 'agent'] as const
export type PluginView = typeof PLUGIN_VIEWS[number]

export function normalizePluginView(value: string | undefined): PluginView {
  return PLUGIN_VIEWS.includes(value as PluginView) ? (value as PluginView) : 'overview'
}

// The capture sample rate is stored as a 0..1 fraction and edited as percent.
export function sampleRateToPercent(rate: number | undefined): number {
  if (typeof rate !== 'number' || !Number.isFinite(rate)) return 0
  return Math.round(Math.min(1, Math.max(0, rate)) * 1000) / 10
}

export function sampleRateFromPercent(percent: number): number {
  if (!Number.isFinite(percent)) return 0
  return Math.min(100, Math.max(0, percent)) / 100
}

export function parsePluginConfigText(text: string): Record<string, unknown> | null {
  try {
    const value = JSON.parse(text.trim() || '{}')
    return value && typeof value === 'object' && !Array.isArray(value) ? value : null
  } catch {
    return null
  }
}

// BPS plugin config (proxy BPSConfig). 0 / empty means the server default.
// Number fields say what 0 means: the server default (defaultValue) or off.
export type PluginConfigField =
  | { key: string; kind: 'number'; min: number; max: number; hint?: boolean; defaultValue?: number; zeroMeans?: 'default' | 'off' }
  | { key: string; kind: 'boolean'; defaultValue?: boolean; hint?: boolean }
  | { key: string; kind: 'text'; hint?: boolean; placeholder?: string }
  | { key: string; kind: 'list'; defaultValue: string[]; hint?: boolean }

export const bpsConfigFields: PluginConfigField[] = [
  { key: 'word_user_agent', kind: 'text', hint: true },
  { key: 'round_convergence_limit', kind: 'number', min: 0, max: 1000000, defaultValue: 100, zeroMeans: 'default' },
  { key: 'round_task_lifetime_hours', kind: 'number', min: 0, max: 8760, defaultValue: 24, zeroMeans: 'default' },
  { key: 'turn_round_limit', kind: 'number', min: 0, max: 1000000, defaultValue: 100, zeroMeans: 'default' },
  { key: 'turn_task_lifetime_hours', kind: 'number', min: 0, max: 8760, defaultValue: 24, zeroMeans: 'default' },
  { key: 'attachment_request_concurrency', kind: 'number', min: 0, max: 64, defaultValue: 15, zeroMeans: 'default' },
  { key: 'attachment_instance_concurrency', kind: 'number', min: 0, max: 1024, defaultValue: 64, zeroMeans: 'default' },
  { key: 'attachment_account_concurrency', kind: 'number', min: 0, max: 1024, defaultValue: 15, zeroMeans: 'default' },
  { key: 'bps_models', kind: 'list', defaultValue: ['gpt-5.6-*', 'gpt-6-*'], hint: true },
  { key: 'bps_only_models', kind: 'list', defaultValue: ['gpt-6-*'], hint: true },
  { key: 'bps_policy_conversation_mark', kind: 'boolean', defaultValue: false, hint: true },
  { key: 'bps_probe_model', kind: 'text', hint: true, placeholder: 'gpt-6-sol' },
  { key: 'bps_min_usable_accounts', kind: 'number', min: 0, max: 1000, hint: true, defaultValue: 2, zeroMeans: 'default' },
  { key: 'bps_account_max_concurrency', kind: 'number', min: 0, max: 100, hint: true, zeroMeans: 'off' },
  { key: 'bps_account_request_budget', kind: 'number', min: 0, max: 10000000, hint: true, zeroMeans: 'off' },
  { key: 'bps_account_budget_window', kind: 'text', hint: true, placeholder: '24h' },
  { key: 'policy_block_threshold', kind: 'number', min: 0, max: 100, hint: true, defaultValue: 3, zeroMeans: 'default' },
  { key: 'bps_policy_cooldown_ladder', kind: 'list', defaultValue: ['2m', '10m', '30m', '2h'], hint: true },
  { key: 'capture_retention_hours', kind: 'number', min: 0, max: 12, hint: true, defaultValue: 6, zeroMeans: 'default' },
  { key: 'capture_error_retention_hours', kind: 'number', min: 0, max: 12, hint: true, defaultValue: 12, zeroMeans: 'default' },
  { key: 'attachment_429_fallback', kind: 'boolean', hint: true },
  { key: 'exclude_failures_from_native_health', kind: 'boolean', defaultValue: true, hint: true },
  { key: 'persist_heuristic_affinity', kind: 'boolean', defaultValue: true, hint: true },
  { key: 'image_trim_default', kind: 'boolean', defaultValue: true, hint: true },
]

// bpsConfigGroups lays the BPS config form out in labeled cards; every field
// of bpsConfigFields belongs to exactly one group.
export const bpsConfigGroups: Array<{ key: string; fields: string[] }> = [
  { key: 'routing', fields: ['bps_models', 'bps_only_models', 'bps_probe_model'] },
  {
    key: 'protection',
    fields: [
      'bps_account_max_concurrency', 'bps_account_request_budget', 'bps_account_budget_window', 'bps_min_usable_accounts',
      'policy_block_threshold', 'bps_policy_cooldown_ladder', 'bps_policy_conversation_mark', 'exclude_failures_from_native_health',
    ],
  },
  { key: 'attachments', fields: ['attachment_request_concurrency', 'attachment_instance_concurrency', 'attachment_account_concurrency', 'attachment_429_fallback', 'image_trim_default'] },
  { key: 'identity', fields: ['word_user_agent', 'round_convergence_limit', 'round_task_lifetime_hours', 'turn_round_limit', 'turn_task_lifetime_hours', 'persist_heuristic_affinity'] },
  { key: 'captures', fields: ['capture_retention_hours', 'capture_error_retention_hours'] },
]

// pluginConfigListText is a list field's editable text: saved lists are
// comma-joined, a draft string is shown as typed.
export function pluginConfigListText(config: Record<string, unknown>, key: string): string {
  const value = config[key]
  if (typeof value === 'string') return value
  return Array.isArray(value) ? value.join(', ') : ''
}

// normalizePluginConfig turns list drafts into arrays before saving; an empty
// list is omitted so the server default applies.
export function normalizePluginConfig(config: Record<string, unknown>, fields: PluginConfigField[]): Record<string, unknown> {
  const out = { ...config }
  for (const field of fields) {
    if (field.kind !== 'list') continue
    const items = pluginConfigListText(out, field.key).split(',').map((item) => item.trim()).filter(Boolean)
    if (items.length) out[field.key] = items
    else delete out[field.key]
  }
  return out
}

// pluginConfigBoolean is a boolean field's effective value: absent means the
// server default.
export function pluginConfigBoolean(config: Record<string, unknown>, field: PluginConfigField): boolean {
  const value = config[field.key]
  if (typeof value === 'boolean') return value
  return field.kind === 'boolean' && field.defaultValue === true
}

// plugin_meta is plugin-owned JSON; show its scalar fields compactly.
export function pluginMetaSummary(raw: string | undefined): string {
  if (!raw) return ''
  try {
    const value = JSON.parse(raw)
    if (!value || typeof value !== 'object' || Array.isArray(value)) return ''
    return Object.entries(value)
      .filter(([, v]) => typeof v === 'string' || typeof v === 'number' || typeof v === 'boolean')
      .map(([k, v]) => `${k}=${v}`)
      .join(' · ')
  } catch {
    return ''
  }
}

// Log-agent source of a plugin's capture store (registered by admin).
export function pluginCaptureSource(pluginId: string): string {
  return `${pluginId}.captures`
}

// Capture evidence IDs are "cap:<id>".
export function captureIdFromEvidence(evidenceId: string): number | null {
  const match = /^cap:(\d+)$/.exec(evidenceId.trim())
  return match ? Number(match[1]) : null
}

export function pluginCaptureAgentFilters(filters: { requestId: string; accountId: string; status: string; direction: string }): Record<string, string> {
  const out: Record<string, string> = {}
  if (filters.requestId.trim()) out.request_id = filters.requestId.trim()
  if (Number(filters.accountId) > 0) out.account_id = String(Number(filters.accountId))
  if (filters.status.trim() !== '' && Number.isInteger(Number(filters.status))) out.status = String(Number(filters.status))
  if (filters.direction) out.direction = filters.direction
  return out
}

// Cooldown reasons the BPS plugin reports (proxy/bps_account_state.go).
export const PLUGIN_COOLING_REASONS = ['bps_rate_limited', 'bps_policy_blocked'] as const

export function pluginCoolingReasonKey(reason: string | undefined): string {
  return `plugins.coolingReasons.${reason || 'unknown'}`
}

// Manual capture purge modes (POST /plugins/:id/captures/purge).
export const CAPTURE_PURGE_MODES = ['older_than', 'errors_only', 'all'] as const

// formatCaptureBytes renders a byte count as B / KB / MB / GB.
export function formatCaptureBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 1024) return `${Math.max(0, Math.round(bytes || 0))} B`
  const units = ['KB', 'MB', 'GB', 'TB']
  let value = bytes / 1024
  let unit = 0
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024
    unit++
  }
  return `${value.toFixed(value >= 100 ? 0 : 1)} ${units[unit]}`
}

// formatBlockDuration renders seconds as "2d 4h", "4h 32m", "5m 03s" or "42s".
export function formatBlockDuration(seconds: number): string {
  const total = Math.max(0, Math.floor(seconds || 0))
  const days = Math.floor(total / 86400)
  const hours = Math.floor((total % 86400) / 3600)
  const minutes = Math.floor((total % 3600) / 60)
  const secs = total % 60
  if (days > 0) return `${days}d ${hours}h`
  if (hours > 0) return `${hours}h ${String(minutes).padStart(2, '0')}m`
  if (minutes > 0) return `${minutes}m ${String(secs).padStart(2, '0')}s`
  return `${secs}s`
}

// formatWindowLabel renders a counter window: whole hours as "24h", anything
// else like formatBlockDuration.
export function formatWindowLabel(seconds: number): string {
  const total = Math.max(0, Math.floor(seconds || 0))
  if (total > 0 && total % 3600 === 0) return `${total / 3600}h`
  return formatBlockDuration(total)
}

// liveElapsedSeconds advances a server-computed elapsed time by the time
// passed since it was fetched.
export function liveElapsedSeconds(elapsedSeconds: number, fetchedAtMs: number, nowMs: number): number {
  return elapsedSeconds + Math.max(0, Math.floor((nowMs - fetchedAtMs) / 1000))
}

// secondsUntil is the whole seconds from now until iso (0 when past or unset).
export function secondsUntil(iso: string | undefined, nowMs: number): number {
  if (!iso) return 0
  const at = Date.parse(iso)
  return Number.isFinite(at) ? Math.max(0, Math.ceil((at - nowMs) / 1000)) : 0
}

// BPS dashboard account states (admin/bps_dashboard.go).
export const BPS_ACCOUNT_STATES = ['active', 'policy_blocked', 'rate_cooling', 'budget_exhausted'] as const

// formatSuccessRate renders a 0..1 rate as a percentage ("—" without requests).
export function formatSuccessRate(rate: number, requests: number): string {
  if (!requests) return '—'
  return `${(Math.round(rate * 1000) / 10).toFixed(1)}%`
}

// Activity bars: the fill (0-100) of value against its limit, or against the
// busiest account when there is no limit.
export function activityBarPercent(value: number, limit: number, busiest: number): number {
  const scale = limit > 0 ? limit : Math.max(busiest, 1)
  return Math.max(0, Math.min(100, Math.round((value / scale) * 100)))
}

export type CapacityFill = 'idle' | 'live' | 'over'

// capacityFill is how a capacity bar (in flight against the cap, budget used
// against the limit) fills: 'over' at or past a set limit (destructive),
// 'live' while in use, 'idle' at zero.
export function capacityFill(value: number, limit: number): CapacityFill {
  if (limit > 0 && value >= limit) return 'over'
  return value > 0 ? 'live' : 'idle'
}

// hasTime reports whether iso is a real timestamp: set, parseable and after
// the Unix epoch (Go's zero time 0001-01-01 is "unset", not a date).
export function hasTime(iso: string | undefined | null): iso is string {
  if (!iso) return false
  const at = Date.parse(iso)
  return Number.isFinite(at) && at > 0
}

// activeUntil reports whether iso is a real time still in the future, i.e.
// a cooldown that is actually running.
export function activeUntil(iso: string | undefined | null, nowMs: number): iso is string {
  return hasTime(iso) && Date.parse(iso) > nowMs
}

// BPS traffic chart ranges and their bucket grids (lib/timeRange; the
// server buckets the same way, admin/bps_dashboard.go).
export const BPS_TRAFFIC_RANGES = ['1h', '24h'] as const
export type BPSTrafficRange = (typeof BPS_TRAFFIC_RANGES)[number]

export interface BPSTrafficSeriesPoint {
  bucket: string
  label: string
  fullLabel: string
  requests: number
  succeeded: number
  errors4xx: number
  errors5xx: number
  org429: number
  account429: number
  policyBlocked: number
}

const BPS_TRAFFIC_BUCKETS: Record<BPSTrafficRange, { bucketMinutes: number; bucketCount: number }> = {
  '1h': { bucketMinutes: 1, bucketCount: 60 },
  '24h': { bucketMinutes: 30, bucketCount: 48 },
}

// bpsTrafficSeries lays server buckets onto the range's full grid ending at
// nowMs (empty buckets are zero), so the chart shows gaps as gaps.
export function bpsTrafficSeries(points: readonly BPSTrafficPoint[], range: BPSTrafficRange, nowMs: number): BPSTrafficSeriesPoint[] {
  const { bucketMinutes, bucketCount } = BPS_TRAFFIC_BUCKETS[range]
  const size = bucketMinutes * 60_000
  const last = Math.floor(nowMs / size) * size
  const first = last - (bucketCount - 1) * size
  const pad = (n: number) => String(n).padStart(2, '0')
  const series = Array.from({ length: bucketCount }, (_, i): BPSTrafficSeriesPoint => {
    const at = new Date(first + i * size)
    const clock = `${pad(at.getHours())}:${pad(at.getMinutes())}`
    return {
      bucket: at.toISOString(), label: clock, fullLabel: `${pad(at.getMonth() + 1)}-${pad(at.getDate())} ${clock}`,
      requests: 0, succeeded: 0, errors4xx: 0, errors5xx: 0, org429: 0, account429: 0, policyBlocked: 0,
    }
  })
  for (const point of points) {
    const at = Date.parse(point.bucket)
    if (!Number.isFinite(at)) continue
    const index = Math.floor((at - first) / size)
    if (index < 0 || index >= bucketCount) continue
    const slot = series[index]
    slot.requests += point.requests
    slot.succeeded += point.succeeded
    slot.errors4xx += point.errors_4xx
    slot.errors5xx += point.errors_5xx
    slot.org429 += point.org_rate_limited
    slot.account429 += point.rate_limited
    slot.policyBlocked += point.policy_blocked
  }
  return series
}

// bpsHealthTimeline adapts BPS buckets to SystemHealthBar's timeline, which
// rates success as requests minus 4xx and 5xx: failures that are neither
// (in-stream errors on a 2xx) fold into 5xx so the strip matches Succeeded.
export function bpsHealthTimeline(points: readonly BPSTrafficPoint[]) {
  return points.map((point) => ({
    bucket: point.bucket,
    requests: point.requests,
    avg_latency: 0,
    input_tokens: 0,
    output_tokens: 0,
    reasoning_tokens: 0,
    cached_tokens: 0,
    errors_4xx: point.errors_4xx,
    errors_5xx: Math.max(0, point.requests - point.succeeded - point.errors_4xx),
  }))
}

// BPS_STATE_BADGE_CLASSES tints the account state badge: amber while the
// account waits out a rate limit or its budget window.
export const BPS_STATE_BADGE_CLASSES: Partial<Record<(typeof BPS_ACCOUNT_STATES)[number], string>> = {
  rate_cooling: 'border-transparent bg-amber-500/14 text-amber-700 dark:bg-amber-500/20 dark:text-amber-300',
  budget_exhausted: 'border-transparent bg-amber-500/14 text-amber-700 dark:bg-amber-500/20 dark:text-amber-300',
}

// secondsSince is the whole seconds since iso (undefined when unset).
export function secondsSince(iso: string | undefined, nowMs: number): number | undefined {
  if (!iso) return undefined
  const at = Date.parse(iso)
  return Number.isFinite(at) ? Math.max(0, Math.floor((nowMs - at) / 1000)) : undefined
}
