export type CodexUAKind = 'codex-tui' | 'codex-desktop' | 'codex-vscode' | 'codex-exec' | 'custom'
export const CODEX_UA_KINDS: CodexUAKind[] = ['codex-tui', 'codex-desktop', 'codex-vscode', 'codex-exec', 'custom']

const PROFILE_KEYS = ['raw_user_agent', 'client_name', 'client_version', 'os_name', 'os_version', 'arch', 'terminal', 'app_name', 'app_version'] as const
export type CodexUserAgentProfile = Partial<Record<typeof PROFILE_KEYS[number], string>>
export type CodexUserAgentConfig = CodexUserAgentProfile & {
  client_kind?: string
  mode?: string
  pool_mix?: Record<string, number>
  profiles?: Partial<Record<CodexUAKind, CodexUserAgentProfile>>
}

export const inferCodexUAKind = (clientName?: string): CodexUAKind => {
  const name = (clientName ?? '').trim().toLowerCase()
  if (!name || name === 'codex-tui') return 'codex-tui'
  if (name === 'codex desktop') return 'codex-desktop'
  if (name === 'codex_vscode') return 'codex-vscode'
  if (name === 'codex_exec') return 'codex-exec'
  return 'custom'
}

const isObject = (value: unknown): value is Record<string, unknown> =>
  value !== null && typeof value === 'object' && !Array.isArray(value)

const profileOf = (value: Record<string, unknown>): CodexUserAgentProfile => {
  const profile: CodexUserAgentProfile = {}
  for (const key of PROFILE_KEYS) {
    if (typeof value[key] === 'string' && value[key] !== '') profile[key] = value[key]
  }
  return profile
}

export const parseCodexUserAgentConfig = (value?: string): CodexUserAgentConfig => {
  try {
    const parsed: unknown = JSON.parse(value || '{}')
    if (!isObject(parsed)) return {}
    const config: CodexUserAgentConfig = profileOf(parsed)
    for (const key of ['client_kind', 'mode'] as const) {
      if (typeof parsed[key] === 'string') config[key] = parsed[key]
    }
    if (isObject(parsed.pool_mix)) {
      config.pool_mix = {}
      for (const [kind, weight] of Object.entries(parsed.pool_mix)) {
        if (typeof weight === 'number' && Number.isFinite(weight)) config.pool_mix[kind] = weight
      }
    }
    if (isObject(parsed.profiles)) {
      config.profiles = {}
      for (const kind of CODEX_UA_KINDS) {
        if (isObject(parsed.profiles[kind])) config.profiles[kind] = profileOf(parsed.profiles[kind])
      }
    }
    return config
  } catch {
    return {}
  }
}

// Preserve in-progress spaces and incomplete versions; normalization and
// validation belong to the explicit save, not each keystroke or tab change.
export const serializeCodexUserAgentConfig = (config: CodexUserAgentConfig) => JSON.stringify(config)

export const reconcileCodexIdentitySave = (current: string, submitted: string, saved: string) =>
  current === submitted ? saved : current

export const selectedCodexUAKind = (config: CodexUserAgentConfig): CodexUAKind =>
  CODEX_UA_KINDS.includes(config.client_kind as CodexUAKind)
    ? config.client_kind as CodexUAKind
    : inferCodexUAKind(config.client_name)

export const updateCodexIdentityDraft = (config: CodexUserAgentConfig, patch: Partial<CodexUserAgentConfig>): CodexUserAgentConfig => {
  const next = { ...config, ...patch }
  const kind = selectedCodexUAKind(next)
  return { ...next, profiles: { ...config.profiles, [kind]: profileOf(next) } }
}

export const switchCodexIdentityDraft = (config: CodexUserAgentConfig, kind: CodexUAKind): CodexUserAgentConfig => {
  const current = updateCodexIdentityDraft(config, {})
  const next: CodexUserAgentConfig = { ...current, client_kind: kind }
  for (const key of PROFILE_KEYS) delete next[key]
  return { ...next, ...current.profiles?.[kind] }
}
