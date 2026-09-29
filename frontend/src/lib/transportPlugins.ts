// Transport plugin page helpers (pure, unit-tested).

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
export type PluginConfigField =
  | { key: string; kind: 'number'; min: number; max: number; hint?: boolean }
  | { key: string; kind: 'boolean'; defaultValue?: boolean; hint?: boolean }
  | { key: string; kind: 'text'; hint?: boolean }
  | { key: string; kind: 'list'; defaultValue: string[]; hint?: boolean }

export const bpsConfigFields: PluginConfigField[] = [
  { key: 'word_user_agent', kind: 'text' },
  { key: 'round_convergence_limit', kind: 'number', min: 0, max: 1000000 },
  { key: 'round_task_lifetime_hours', kind: 'number', min: 0, max: 8760 },
  { key: 'turn_round_limit', kind: 'number', min: 0, max: 1000000 },
  { key: 'turn_task_lifetime_hours', kind: 'number', min: 0, max: 8760 },
  { key: 'attachment_request_concurrency', kind: 'number', min: 0, max: 64 },
  { key: 'attachment_instance_concurrency', kind: 'number', min: 0, max: 1024 },
  { key: 'attachment_account_concurrency', kind: 'number', min: 0, max: 1024 },
  { key: 'bps_models', kind: 'list', defaultValue: ['gpt-5.6-*', 'gpt-6-*'], hint: true },
  { key: 'bps_only_models', kind: 'list', defaultValue: ['gpt-6-*'], hint: true },
  { key: 'bps_account_max_concurrency', kind: 'number', min: 0, max: 100, hint: true },
  { key: 'policy_block_threshold', kind: 'number', min: 0, max: 100 },
  { key: 'bps_policy_cooldown_ladder', kind: 'list', defaultValue: ['2m', '10m', '30m', '2h'], hint: true },
  { key: 'capture_retention_hours', kind: 'number', min: 0, max: 12, hint: true },
  { key: 'capture_error_retention_hours', kind: 'number', min: 0, max: 12, hint: true },
  { key: 'attachment_429_fallback', kind: 'boolean' },
  { key: 'exclude_failures_from_native_health', kind: 'boolean', defaultValue: true, hint: true },
  { key: 'persist_heuristic_affinity', kind: 'boolean', defaultValue: true, hint: true },
  { key: 'image_trim_default', kind: 'boolean', defaultValue: true, hint: true },
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
