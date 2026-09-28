import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import { Link } from 'react-router-dom'
import { AlertTriangle, Bot, RefreshCw, Sparkles } from 'lucide-react'
import { api } from '../api'
import type { LogAgentConfig, LogAgentContextStats, LogAgentRun } from '../types'
import { formatBeijingTime } from '../utils/time'
import { getErrorMessage } from '../utils/error'
import { compactFilters, formatConfidence, logAgentLanguage, runFindings } from '../lib/logAgent'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'
import { cn } from '@/lib/utils'

// LogAgentPanel 是日志分析 Agent 的通用入口：调用方给出来源（source）与记录引用/筛选条件，
// 面板负责触发分析、展示结构化结论与该来源的历史记录。运维错误页与各插件页共用。
export interface LogAgentPanelProps {
  /** 后端注册的来源名，如 ops_errors、usage_logs 或插件自己的来源。 */
  source: string
  /** 记录引用（请求 ID、抓包 ID 等），含义由来源决定。 */
  refs?: string[]
  /** 页面筛选条件，空值会被忽略。 */
  filters?: Record<string, string | undefined>
  /** 时间范围（RFC3339）；getRange 优先，在点击分析时才求值，避免相对时间范围过期。 */
  start?: string
  end?: string
  getRange?: () => { start: string; end: string }
  title?: string
  description?: string
  /** 不包 Card，嵌入对话框等已有容器时使用。 */
  bare?: boolean
  /** 是否展示该来源的历史分析记录。 */
  showHistory?: boolean
  /** 点击证据 ID 时回调（例如打开对应日志详情）；不传则证据只作展示。 */
  onEvidenceClick?: (evidenceId: string) => void
  className?: string
}

const HISTORY_LIMIT = 10

export default function LogAgentPanel({
  source,
  refs,
  filters,
  start,
  end,
  getRange,
  title,
  description,
  bare = false,
  showHistory = true,
  onEvidenceClick,
  className,
}: LogAgentPanelProps) {
  const { t, i18n } = useTranslation()
  const [config, setConfig] = useState<LogAgentConfig | null>(null)
  const [configError, setConfigError] = useState('')
  const [focus, setFocus] = useState('')
  const [running, setRunning] = useState(false)
  const [error, setError] = useState('')
  const [run, setRun] = useState<LogAgentRun | null>(null)
  const [history, setHistory] = useState<LogAgentRun[]>([])

  const loadHistory = useCallback(async () => {
    if (!showHistory) return
    try {
      const result = await api.listLogAgentRuns({ source, limit: HISTORY_LIMIT })
      setHistory(result.runs ?? [])
    } catch {
      /* 历史记录加载失败不影响分析 */
    }
  }, [showHistory, source])

  useEffect(() => {
    let cancelled = false
    api.getLogAgentConfig()
      .then((result) => { if (!cancelled) setConfig(result.config) })
      .catch((err) => { if (!cancelled) setConfigError(getErrorMessage(err)) })
    void loadHistory()
    return () => { cancelled = true }
  }, [loadHistory])

  const ready = Boolean(config?.enabled && config.model)

  const analyze = async () => {
    setRunning(true)
    setError('')
    try {
      const range = getRange ? getRange() : { start: start ?? '', end: end ?? '' }
      const result = await api.analyzeLogAgent({
        source,
        refs: refs?.filter(Boolean),
        filters: compactFilters(filters),
        start: range.start || undefined,
        end: range.end || undefined,
        focus: focus.trim() || undefined,
        language: logAgentLanguage(i18n.resolvedLanguage || i18n.language),
      })
      setRun(result.run)
    } catch (err) {
      setError(getErrorMessage(err))
    } finally {
      setRunning(false)
      void loadHistory()
    }
  }

  const historyOptions = useMemo(() => [
    { value: '', label: t('logAgent.historyPlaceholder') },
    ...history.map((item) => ({
      value: String(item.id),
      label: `${formatBeijingTime(item.created_at)} · ${t(`logAgent.status.${item.status}`)}`,
    })),
  ], [history, t])

  const header = (
    <div className="flex flex-wrap items-start justify-between gap-3">
      <div className="flex min-w-0 items-start gap-3">
        <div className="flex size-8 shrink-0 items-center justify-center rounded-lg bg-muted/70 text-muted-foreground ring-1 ring-inset ring-border/60" aria-hidden="true">
          <Bot className="size-4" />
        </div>
        <div className="min-w-0">
          <h3 className="text-sm font-semibold text-foreground sm:text-[15px]">{title ?? t('logAgent.title')}</h3>
          <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{description ?? t('logAgent.description')}</p>
        </div>
      </div>
      <Button size="sm" onClick={() => void analyze()} disabled={!ready || running}>
        {running ? <RefreshCw className="size-3.5 animate-spin" /> : <Sparkles className="size-3.5" />}
        {running ? t('logAgent.analyzing') : t('logAgent.analyze')}
      </Button>
    </div>
  )

  const body = (
    <div className={cn('space-y-4', className)}>
      {header}
      {configError ? (
        <p className="text-xs text-destructive">{t('logAgent.configLoadFailed', { message: configError })}</p>
      ) : config && !ready ? (
        <div className="flex flex-wrap items-center gap-2 rounded-lg border border-border/70 bg-muted/35 px-3 py-2 text-xs text-muted-foreground">
          <span>{config.enabled ? t('logAgent.modelMissing') : t('logAgent.disabledHint')}</span>
          <Link to="/settings?tab=general" className="font-medium text-foreground underline-offset-4 hover:underline">
            {t('logAgent.openSettings')}
          </Link>
        </div>
      ) : null}
      <div className="flex flex-wrap items-center gap-2">
        <Input
          className="h-8 min-w-0 flex-1 text-[13px]"
          value={focus}
          maxLength={500}
          placeholder={t('logAgent.focusPlaceholder')}
          onChange={(event) => setFocus(event.target.value)}
        />
        {showHistory && history.length > 0 ? (
          <Select
            className="w-64 max-sm:w-full"
            compact
            aria-label={t('logAgent.history')}
            value={run ? String(run.id) : ''}
            onValueChange={(value) => setRun(history.find((item) => String(item.id) === value) ?? null)}
            options={historyOptions}
          />
        ) : null}
      </div>
      <p className="font-mono text-xs text-muted-foreground">
        {t('logAgent.scope', { source, refs: refs?.filter(Boolean).length ?? 0 })}
      </p>
      {error ? (
        <div className="flex items-start gap-2 rounded-lg border border-destructive/25 bg-destructive/5 px-3 py-2 text-xs text-destructive">
          <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
          <span className="min-w-0 break-words">{error}</span>
        </div>
      ) : null}
      {run ? <LogAgentRunView run={run} onEvidenceClick={onEvidenceClick} /> : null}
    </div>
  )

  if (bare) return body
  return (
    <Card className="gap-0 py-0">
      <CardContent className="p-4.5 sm:p-5.5">{body}</CardContent>
    </Card>
  )
}

function LogAgentRunView({ run, onEvidenceClick }: { run: LogAgentRun; onEvidenceClick?: (id: string) => void }) {
  const { t } = useTranslation()
  const findings = runFindings(run)
  const stats = run.context_stats as Partial<LogAgentContextStats>
  return (
    <div className="space-y-4 rounded-lg border border-border/70 bg-card/75 p-4">
      <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
        <Badge variant="outline" className={cn(run.status === 'failed' && 'border-destructive/30 text-destructive')}>
          {t(`logAgent.status.${run.status}`)}
        </Badge>
        {findings ? <Badge variant="outline">{t('logAgent.confidence', { value: formatConfidence(findings.confidence) })}</Badge> : null}
        <span className="font-mono">{run.model}</span>
        <span>·</span>
        <span>{formatBeijingTime(run.created_at)}</span>
        <span>·</span>
        <span className="font-mono tabular-nums">{t('logAgent.stats', {
          records: run.record_count,
          groups: stats.included_groups ?? 0,
          tokens: run.total_tokens,
          seconds: (run.duration_ms / 1000).toFixed(1),
        })}</span>
        {stats.truncated ? <Badge variant="outline">{t('logAgent.truncated')}</Badge> : null}
      </div>

      {run.status === 'failed' ? (
        <p className="text-sm text-destructive">{run.error_message || t('logAgent.failed')}</p>
      ) : null}
      {findings?.fallback ? (
        <p className="text-xs text-amber-600 dark:text-amber-400">{t('logAgent.fallbackHint')}</p>
      ) : null}
      {findings ? (
        <>
          <p className="whitespace-pre-wrap break-words text-sm leading-relaxed text-foreground">{findings.summary}</p>
          {findings.root_causes.length > 0 ? (
            <FindingSection title={t('logAgent.rootCauses')}>
              {findings.root_causes.map((cause, index) => (
                <li key={`${cause.title}-${index}`} className="space-y-1.5 py-3 first:pt-0 last:pb-0">
                  <div className="flex flex-wrap items-center gap-2">
                    <span className="text-sm font-semibold text-foreground">{cause.title || t('logAgent.untitled')}</span>
                    <Badge variant="outline">{t(`logAgent.category.${cause.category}`)}</Badge>
                    <span className="font-mono text-xs text-muted-foreground">{formatConfidence(cause.confidence)}</span>
                  </div>
                  {cause.detail ? <p className="whitespace-pre-wrap break-words text-xs leading-relaxed text-muted-foreground">{cause.detail}</p> : null}
                  {cause.evidence_ids.length > 0 ? (
                    <div className="flex flex-wrap items-center gap-1.5">
                      <span className="text-xs text-muted-foreground">{t('logAgent.evidence')}</span>
                      {cause.evidence_ids.map((id) => onEvidenceClick ? (
                        <Button key={id} variant="outline" size="xs" className="font-mono" onClick={() => onEvidenceClick(id)}>{id}</Button>
                      ) : (
                        <span key={id} className="font-mono text-xs text-muted-foreground">{id}</span>
                      ))}
                    </div>
                  ) : null}
                </li>
              ))}
            </FindingSection>
          ) : null}
          {findings.suggested_actions.length > 0 ? (
            <FindingSection title={t('logAgent.actions')}>
              {findings.suggested_actions.map((action, index) => (
                <li key={`${action.title}-${index}`} className="space-y-1 py-3 first:pt-0 last:pb-0">
                  <div className="flex flex-wrap items-center gap-2">
                    <Badge variant="outline" className={cn(action.priority === 'high' && 'border-destructive/30 text-destructive')}>
                      {t(`logAgent.priority.${action.priority}`)}
                    </Badge>
                    <span className="text-sm font-medium text-foreground">{action.title || t('logAgent.untitled')}</span>
                  </div>
                  {action.detail ? <p className="whitespace-pre-wrap break-words text-xs leading-relaxed text-muted-foreground">{action.detail}</p> : null}
                </li>
              ))}
            </FindingSection>
          ) : null}
        </>
      ) : null}
    </div>
  )
}

function FindingSection({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div>
      <div className="mb-2 text-[12px] font-semibold uppercase text-muted-foreground">{title}</div>
      <ul className="divide-y divide-border/60">{children}</ul>
    </div>
  )
}
