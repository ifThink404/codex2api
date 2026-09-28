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
  | { key: string; kind: 'number'; min: number; max: number }
  | { key: string; kind: 'boolean' }
  | { key: string; kind: 'text' }

export const bpsConfigFields: PluginConfigField[] = [
  { key: 'word_user_agent', kind: 'text' },
  { key: 'round_convergence_limit', kind: 'number', min: 0, max: 1000000 },
  { key: 'round_task_lifetime_hours', kind: 'number', min: 0, max: 8760 },
  { key: 'turn_round_limit', kind: 'number', min: 0, max: 1000000 },
  { key: 'turn_task_lifetime_hours', kind: 'number', min: 0, max: 8760 },
  { key: 'attachment_request_concurrency', kind: 'number', min: 0, max: 64 },
  { key: 'attachment_instance_concurrency', kind: 'number', min: 0, max: 1024 },
  { key: 'attachment_account_concurrency', kind: 'number', min: 0, max: 1024 },
  { key: 'attachment_429_fallback', kind: 'boolean' },
  { key: 'exclude_failures_from_native_health', kind: 'boolean' },
]

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
