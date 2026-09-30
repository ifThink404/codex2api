// Error badges shared by the ops errors page (and its BPS plugin embed) and
// the usage log: status-class fallback labels, status badge colors and a
// tone per upstream error kind. Badge classes follow the StatTile tone
// tokens (tinted surface, 600 text in light, 300 text in dark).

export type ErrorKindTone = 'rate' | 'policy' | 'availability' | 'info' | 'error'

export const ERROR_KIND_TONES: readonly ErrorKindTone[] = ['rate', 'policy', 'availability', 'info', 'error']

// Known kinds; anything else is classified by errorKindTone's patterns.
const ERROR_KIND_TONE_MAP: Record<string, ErrorKindTone> = {
  bps_rate_limited: 'rate',
  rate_limit: 'rate',
  rate_limited: 'rate',
  rate_limited_model: 'rate',
  org_rate_limited: 'rate',
  usage_limit: 'rate',
  quota_exhausted: 'rate',
  bps_policy_blocked: 'policy',
  basispoints_model_access_changed: 'policy',
  cyber_policy: 'policy',
  bps_account_refused: 'policy',
  forbidden: 'policy',
  bps_unavailable: 'availability',
  bps_model_unavailable: 'availability',
  bps_concurrency_full: 'availability',
  bps_concurrency_limited: 'availability',
  bps_budget_exhausted: 'availability',
  client_closed: 'availability',
  basispoints_cutoff_completed: 'info',
  server_error: 'error',
  upstream_error: 'error',
  upstream_timeout: 'error',
  transport_error: 'error',
  unauthorized: 'error',
  client_error: 'error',
}

// errorKindTone groups an error kind: rate limits amber, policy / access
// rose, availability (no capacity, not an upstream failure) slate, stream
// info blue, and upstream / 5xx / auth failures red.
export function errorKindTone(kind: string | undefined | null): ErrorKindTone {
  const key = (kind ?? '').trim().toLowerCase()
  if (!key) return 'error'
  const known = ERROR_KIND_TONE_MAP[key]
  if (known) return known
  if (/rate_limit|429|quota|usage_limit/.test(key)) return 'rate'
  if (/policy|access|forbidden|refused|safety/.test(key)) return 'policy'
  if (/unavailable|budget|concurrency|cooldown|cooling/.test(key)) return 'availability'
  if (/cutoff|completed/.test(key)) return 'info'
  return 'error'
}

export const ERROR_KIND_TONE_BADGE_CLASSES: Record<ErrorKindTone, string> = {
  rate: 'border-transparent bg-amber-500/14 text-amber-700 dark:bg-amber-500/20 dark:text-amber-300',
  policy: 'border-rose-500/30 bg-rose-500/12 text-rose-700 dark:bg-rose-500/20 dark:text-rose-300',
  availability: 'border-transparent bg-slate-500/14 text-slate-600 dark:bg-slate-500/20 dark:text-slate-300',
  info: 'border-transparent bg-blue-500/12 text-blue-600 dark:bg-blue-500/20 dark:text-blue-300',
  error: 'border-transparent bg-red-500/14 text-red-600 dark:bg-red-500/20 dark:text-red-300',
}

// Text-only tone (for inline labels such as the usage log's error summary).
export const ERROR_KIND_TONE_TEXT_CLASSES: Record<ErrorKindTone, string> = {
  rate: 'text-amber-700 dark:text-amber-300',
  policy: 'text-rose-700 dark:text-rose-300',
  availability: 'text-slate-600 dark:text-slate-300',
  info: 'text-blue-600 dark:text-blue-300',
  error: 'text-red-600 dark:text-red-300',
}

export function errorKindBadgeClassName(kind: string | undefined | null): string {
  return ERROR_KIND_TONE_BADGE_CLASSES[errorKindTone(kind)]
}

// classifyStatus labels an error row that recorded no upstream error kind by
// its status (the server groups by account with the same labels,
// database.UsageErrorKindForStatus).
export function classifyStatus(statusCode: number): string {
  if (statusCode === 401) return 'unauthorized'
  if (statusCode === 403) return 'forbidden'
  if (statusCode === 429) return 'rate_limit'
  if (statusCode === 499) return 'client_closed'
  if (statusCode >= 500) return 'server_error'
  if (statusCode >= 400) return 'client_error'
  return 'error'
}

// errorKindLabel is the row's upstream error kind, or its status class.
export function errorKindLabel(log: { upstream_error_kind?: string; status_code: number }): string {
  return log.upstream_error_kind?.trim() || classifyStatus(log.status_code)
}

// errorStatusBadgeClassName colors an error status code: 401 and 5xx red,
// 499 slate, other 4xx (429 included) amber.
export function errorStatusBadgeClassName(statusCode: number): string {
  if (statusCode === 401 || statusCode >= 500) return ERROR_KIND_TONE_BADGE_CLASSES.error
  if (statusCode === 499) return ERROR_KIND_TONE_BADGE_CLASSES.availability
  return ERROR_KIND_TONE_BADGE_CLASSES.rate
}
