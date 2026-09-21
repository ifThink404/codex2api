export interface UsageRequestDiagnosticDetail {
  request_type: string
  diagnostics: Record<string, unknown> | null
}

export const usageRequestTypes = ['user', 'related_internal', 'independent_internal', 'related_unclassified', 'compaction', 'gateway_internal', 'unknown'] as const
const requestTypes = new Set<string>(usageRequestTypes)

export function usageRequestTypeLabelKey(value?: string): string {
  if (!value) return 'usage.diagnostics.types.not_recorded'
  return `usage.diagnostics.types.${requestTypes.has(value) ? value : 'unknown'}`
}

export function diagnosticRecord(value: unknown): Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : {}
}

export function hasOmittedIncomingDiagnostic(value: unknown): boolean {
  const record = diagnosticRecord(value)
  // Legacy size limiting explicitly replaced the complete ingress map with null.
  // An empty map or a nesting-limit flag alone does not prove that happened.
  return record.truncated === true && record.incoming === null
}

export function accessProgramsDiagnosticValue(value: unknown): { state: string; text?: string } {
  const item = diagnosticRecord(value)
  if (item.state === 'present' && Object.prototype.hasOwnProperty.call(item, 'value')) {
    return { state: 'present', text: JSON.stringify(item.value, null, 2) }
  }
  if (item.state === 'too_large') {
    return { state: 'too_large', text: JSON.stringify({ bytes: item.bytes, sha256: item.sha256 }, null, 2) }
  }
  if (item.state === 'absent' || item.state === 'invalid_json') return { state: item.state }
  return { state: 'not_recorded' }
}

export const compactionMetadataFields = ['implementation', 'trigger', 'reason', 'phase', 'strategy'] as const

export function compactionMetadataDiagnosticValue(value: unknown): { state: string; fields: Record<string, string>; invalidFields: string[] } {
  const item = diagnosticRecord(value)
  const state = typeof item.state === 'string' && ['present', 'absent', 'invalid_metadata', 'invalid_type', 'too_large'].includes(item.state) ? item.state : 'not_recorded'
  const fields: Record<string, string> = {}
  if (state === 'present') {
    for (const field of compactionMetadataFields) {
      if (typeof item[field] === 'string' && item[field]) fields[field] = item[field]
    }
  }
  const invalid = item.invalid_fields
  const invalidFields = Array.isArray(invalid) ? compactionMetadataFields.filter(field => invalid.includes(field)) : []
  return { state, fields, invalidFields }
}

export interface TurnStateDiagnosticRow {
  kind: 'received' | 'upstream' | 'real' | 'alias' | 'hash'
  value: string
  carrier: string
  action?: string
}

export function turnStateDiagnosticRows(value: unknown, outboundIdentity: unknown): TurnStateDiagnosticRow[] {
  const rows: TurnStateDiagnosticRow[] = []
  const seen = new Set<string>()
  const add = (kind: TurnStateDiagnosticRow['kind'], value: unknown, carrier: string, action?: string) => {
    if (typeof value !== 'string' || !value) return
    const key = JSON.stringify([kind, value, carrier, action])
    if (!seen.has(key)) { seen.add(key); rows.push({ kind, value, carrier, action }) }
  }
  const state = diagnosticRecord(value)
  for (const item of Array.isArray(state.events) ? state.events : []) {
    const event = diagnosticRecord(item)
    const carrier = typeof event.carrier === 'string' ? event.carrier : ''
    const action = typeof event.action === 'string' ? event.action : ''
    add('received', event.received, carrier, action)
    // These are mapped/returned real values, not proof they were sent upstream.
    add('real', event.real, carrier, action)
    if (action === 'issued') add('alias', event.alias, carrier, action)
    if (!event.real && !event.received) add('hash', event.real_hash, carrier, action)
  }
  const outbound = diagnosticRecord(outboundIdentity)
  for (const [name, value] of Object.entries(diagnosticRecord(diagnosticRecord(outbound.http).headers))) {
    if (name.toLowerCase() === 'x-codex-turn-state') add('upstream', value, 'HTTP X-Codex-Turn-State')
  }
  add('upstream', diagnosticRecord(diagnosticRecord(outbound.body).client_metadata)['x-codex-turn-state'], 'client_metadata.x-codex-turn-state')
  return rows
}

export function splitOutboundIdentityDiagnostic(value: unknown): { snapshot: Record<string, unknown>; local: Record<string, unknown> } {
  const snapshot: Record<string, unknown> = {}
  const local: Record<string, unknown> = {}
  for (const [key, item] of Object.entries(diagnosticRecord(value))) {
    if (['http', 'ws_handshake', 'body'].includes(key)) snapshot[key] = item
    else local[key] = item
  }
  return { snapshot, local }
}

const clientMetadataFields = new Set([
  'installation_id', 'installationId', 'device_id', 'deviceId',
  'x-codex-installation-id', 'x_codex_installation_id', 'x-device-id', 'x_device_id',
  'client_name', 'client_version', 'os_name', 'os_version', 'arch', 'timezone',
])

const clientHeaderFields = new Set([
  'X-Codex-Installation-Id', 'X-Installation-Id', 'X-Device-Id', 'Oai-Device-Id',
  'User-Agent', 'Originator', 'Version', 'X-Stainless-OS', 'X-Stainless-Arch',
  'X-Stainless-Runtime', 'X-Stainless-Runtime-Version', 'X-Stainless-Package-Version',
])

export function diagnosticClientInfo(incoming: unknown): Record<string, unknown> {
  const result: Record<string, unknown> = {}
  for (const [source, values] of Object.entries(diagnosticRecord(incoming))) {
    if (source.startsWith('_')) continue
    const allowed = source === 'headers' ? clientHeaderFields : clientMetadataFields
    for (const [field, value] of Object.entries(diagnosticRecord(values))) {
      if (allowed.has(field)) result[`${source}.${field}`] = value
    }
  }
  return result
}

export function diagnosticEntries(value: unknown, includeMissing = false): [string, unknown][] {
  const record = diagnosticRecord(value)
  const fields = includeMissing
    ? { thread_source: '', request_kind: '', subagent_kind: '', session_id: '', thread_id: '', parent_thread_id: '', turn_id: '', root_turn_id: '', ...record }
    : record
  return Object.entries(fields).filter(([key]) => !key.startsWith('_'))
}

export function diagnosticOutboundIdentity(value: unknown): Record<string, unknown> {
  const identity = diagnosticRecord(value)
  const result: Record<string, unknown> = {}
  if (identity.truncated === true) result.capture_truncated = true
  if (typeof identity.session_consistency === 'string' && ['matched', 'mismatched', 'missing_header', 'missing_body', 'body_only', 'not_applicable'].includes(identity.session_consistency)) {
    result.session_consistency = identity.session_consistency
  }
  for (const source of ['http', 'ws_handshake', 'body']) {
    const groups = diagnosticRecord(identity[source])
    const names = source === 'body' ? ['client_metadata', 'turn_metadata', 'links'] : ['headers', 'turn_metadata']
    for (const group of names) {
      for (const [field, item] of diagnosticEntries(groups[group])) {
        result[`${source}.${group}.${field}`] = item
      }
    }
  }
  return result
}

export function diagnosticValueText(value: unknown): string | null {
  if (value === null || value === undefined || value === '' || value === '0001-01-01T00:00:00Z') return null
  if (typeof value === 'string') return value
  if (typeof value === 'boolean' || typeof value === 'number') return String(value)
  if (Array.isArray(value)) return value.length ? value.map(String).join(', ') : null
  return JSON.stringify(value)
}

export function diagnosticJSONDisplay(value: unknown, decodeMetadata = false, depth = 0): unknown {
  if (!decodeMetadata || depth > 24 || value === null || typeof value !== 'object') return value
  if (Array.isArray(value)) return value.map((item) => diagnosticJSONDisplay(item, true, depth + 1))
  return Object.fromEntries(Object.entries(value).map(([key, item]) => {
    if (key.toLowerCase() === 'x-codex-turn-metadata' && typeof item === 'string' && item.length <= 16384) {
      try {
        const decoded: unknown = JSON.parse(item)
        if (decoded !== null && typeof decoded === 'object' && !Array.isArray(decoded)) return [key, decoded]
      } catch {
        return [key, item]
      }
    }
    return [key, diagnosticJSONDisplay(item, true, depth + 1)]
  }))
}
