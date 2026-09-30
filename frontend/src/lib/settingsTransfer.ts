import type { ChannelTestSettings, ClaudeGlobalConfig, UpstreamChannel } from '../types'

export interface SettingsBackup {
  format: 'codex2api.settings'
  version: 1
  exported_at: string
  // secrets_included marks a backup exported with the include-secrets opt-in.
  secrets_included?: boolean
  settings: Record<string, unknown>
  sections?: {
    claude?: ClaudeGlobalConfig
    antigravity?: { model_redirects: Record<string, string>; redirect_overrides_effort: boolean }
    channel_tests?: { claude: ChannelTestSettings; antigravity: ChannelTestSettings }
    invite_guide?: { enabled: boolean }
    visible_channels?: { channels: UpstreamChannel[] }
  }
}

export const SETTINGS_BACKUP_MAX_BYTES = 10 * 1024 * 1024

// Service credentials an export carries only on opt-in (server
// settingsExportSecretFields). The import reference is a secret-free export,
// so these keys are accepted as strings even though the reference lacks them.
export const SETTINGS_SECRET_FIELDS = ['github_token', 'prompt_filter_review_api_key', 'image_s3_access_key', 'image_s3_secret_key'] as const

export class SettingsImportError extends Error {
  readonly reason: 'format' | 'field' | 'type'
  readonly field: string
  constructor(reason: 'format' | 'field' | 'type', field = '') {
    super(`${reason}: ${field}`)
    this.reason = reason
    this.field = field
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
}

// Use the target server's portable write contract, not the file, as authority.
// Keep explicit false/zero/empty values; missing keys mean preserve the target.
export function parseSettingsBackup(text: string, target: SettingsBackup): SettingsBackup {
  let parsed: unknown
  try { parsed = JSON.parse(text) } catch { throw new SettingsImportError('format') }
  if (!isRecord(parsed) || parsed.format !== 'codex2api.settings' || parsed.version !== 1 || !isRecord(parsed.settings) ||
    (parsed.secrets_included !== undefined && typeof parsed.secrets_included !== 'boolean')) {
    throw new SettingsImportError('format')
  }
  const validateFields = (value: unknown, reference: Record<string, unknown>, prefix: string) => {
    if (!isRecord(value)) throw new SettingsImportError('type', prefix)
    for (const [key, item] of Object.entries(value)) {
      const path = `${prefix}.${key}`
      const secret = prefix === 'settings' && (SETTINGS_SECRET_FIELDS as readonly string[]).includes(key)
      if ((!secret && !Object.prototype.hasOwnProperty.call(reference, key)) || ['__proto__', 'prototype', 'constructor', 'admin_secret'].includes(key)) throw new SettingsImportError('field', path)
      const sample = Object.prototype.hasOwnProperty.call(reference, key) ? reference[key] : ''
      if (item === null || typeof item !== typeof sample || Array.isArray(item) !== Array.isArray(sample) || (typeof item === 'number' && !Number.isFinite(item))) {
        throw new SettingsImportError('type', path)
      }
    }
  }
  validateFields(parsed.settings, target.settings, 'settings')
  if (parsed.sections !== undefined) {
    validateFields(parsed.sections, target.sections ?? {}, 'sections')
    for (const [name, value] of Object.entries(parsed.sections as Record<string, unknown>)) {
      const sample = target.sections?.[name as keyof NonNullable<SettingsBackup['sections']>]
      validateFields(value, sample as unknown as Record<string, unknown>, `sections.${name}`)
    }
  }
  if (!Object.keys(parsed.settings).length && !Object.keys(parsed.sections ?? {}).length) throw new SettingsImportError('format')
  return parsed as unknown as SettingsBackup
}
