import type { LogAgentFindings, LogAgentRun } from '../types'

// 日志分析 Agent 前端的纯函数：从持久化记录取结论、格式化置信度、把页面筛选映射成后端 filters。

// runFindings 返回可渲染的结论；失败记录的 findings 是空对象，返回 null。
export function runFindings(run: LogAgentRun | null | undefined): LogAgentFindings | null {
  const findings = run?.findings as Partial<LogAgentFindings> | undefined
  if (!findings || typeof findings.summary !== 'string') return null
  if (!findings.summary && !findings.fallback) return null
  return {
    summary: findings.summary,
    root_causes: Array.isArray(findings.root_causes) ? findings.root_causes : [],
    suggested_actions: Array.isArray(findings.suggested_actions) ? findings.suggested_actions : [],
    confidence: typeof findings.confidence === 'number' ? findings.confidence : 0,
    fallback: Boolean(findings.fallback),
  }
}

export function formatConfidence(value: number | undefined): string {
  if (typeof value !== 'number' || !Number.isFinite(value) || value <= 0) return '-'
  return `${Math.round(Math.min(value, 1) * 100)}%`
}

// logAgentLanguage 把 i18n 语言收敛成后端认识的 zh / zh-TW / en。
export function logAgentLanguage(language: string | undefined): string {
  const lower = (language || '').toLowerCase()
  if (lower === 'zh-tw' || lower === 'zh-hk' || lower === 'zh-hant') return 'zh-TW'
  if (lower.startsWith('zh')) return 'zh'
  return 'en'
}

// compactFilters 去掉空值，后端只接收非空的字符串条件。
export function compactFilters(filters: Record<string, string | undefined> | undefined): Record<string, string> {
  const result: Record<string, string> = {}
  for (const [key, value] of Object.entries(filters ?? {})) {
    const trimmed = (value ?? '').trim()
    if (trimmed) result[key] = trimmed
  }
  return result
}

// opsErrorLogAgentFilters 把运维错误页的筛选状态映射成 ops_errors 来源的 filters（与 /ops/errors 参数同名）。
export function opsErrorLogAgentFilters(params: {
  status?: string
  errorKind?: string
  endpoint?: string
  apiKeyId?: string
  stream?: string
  q?: string
  transport?: string
  accountId?: string
  retry?: string
  timeout?: string
}): Record<string, string> {
  return compactFilters({
    transport: params.transport,
    account_id: params.accountId,
    retry: params.retry,
    timeout: params.timeout,
    status: params.status,
    error_kind: params.errorKind,
    endpoint: params.endpoint,
    api_key_id: params.apiKeyId,
    stream: params.stream,
    q: params.q,
  })
}

// usageLogIdFromEvidence 解析内置来源的证据 ID（usage:<id>），其它来源返回 null。
export function usageLogIdFromEvidence(evidenceId: string): number | null {
  const match = /^usage:(\d+)$/.exec(evidenceId.trim())
  return match ? Number(match[1]) : null
}
