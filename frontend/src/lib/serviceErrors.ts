export const SERVICE_ERROR_STAGES = ['authentication', 'rate_limit', 'policy', 'dispatch', 'validation', 'internal'] as const

export interface ServiceErrorEvent {
  group?: { key: string; count: number; first_seen: string; last_seen: string }
  id: string
  created_at: string
  request_id: string
  newapi_request_id?: string
  newapi_identity_verified?: boolean
  newapi_user_id?: string
  newapi_user_name?: string
  status_code: number
  code: string
  error_type: string
  message: string
  stage: string
  method: string
  endpoint: string
  transport: string
  model?: string
  duration_ms: number
  api_key_id?: number
  api_key_name?: string
  thread_source?: string
  request_kind?: string
  thread_id?: string
  client_info?: Record<string, string>
  tool_protocol?: Record<string, unknown>
}

export interface ServiceErrorPage {
  grouped?: boolean
  items: ServiceErrorEvent[]
  next_cursor?: string
  summary: { total: number; status_429: number; status_4xx: number; status_5xx: number; groups?: number }
  collector: { pending: number; written: number; dropped: number; write_failures: number; capacity: number; retention_days: number; max_rows: number }
}

export interface ServiceErrorQuery {
  grouped?: boolean
  group_key?: string
  start: string
  end: string
  status?: string
  stage?: string
  request_id?: string
  cursor?: string
}

export function serviceErrorSearchParams(query: ServiceErrorQuery): string {
  const search = new URLSearchParams({ start: query.start, end: query.end, limit: '20' })
  if (query.grouped !== undefined) search.set('grouped', String(query.grouped))
  for (const key of ['status', 'stage', 'request_id', 'cursor', 'group_key'] as const) {
    const value = query[key]?.trim()
    if (value) search.set(key, value)
  }
  return search.toString()
}

export function serviceErrorCollectorHasLoss(collector: Pick<ServiceErrorPage['collector'], 'dropped' | 'write_failures'>): boolean {
  return collector.dropped > 0 || collector.write_failures > 0
}

export function serviceErrorNewAPIUserLabel(event: Pick<ServiceErrorEvent, 'newapi_identity_verified' | 'newapi_user_name' | 'newapi_user_id'>): string {
  if (!event.newapi_identity_verified) return ''
  const name = event.newapi_user_name?.trim() || ''
  const userID = event.newapi_user_id?.trim() || ''
  return [name, userID ? `#${userID}` : ''].filter(Boolean).join(' ')
}
