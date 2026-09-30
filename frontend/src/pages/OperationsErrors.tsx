import { Fragment, useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import {
  AlertCircle,
  ChevronDown,
  ChevronRight,
  Clock3,
  Filter,
  Copy,
  Download,
  RefreshCw,
  RotateCcw,
  Search,
  ServerCrash,
  ShieldAlert,
  TimerReset,
  X,
} from 'lucide-react'
import { api } from '../api'
import OpsTabs from '../components/OpsTabs'
import LogAgentPanel from '../components/LogAgentPanel'
import PageHeader from '../components/PageHeader'
import { StatTile } from '../components/StatTile'
import { SegmentedTabs } from '../components/SegmentedTabs'
import Pagination from '../components/Pagination'
import StateShell from '../components/StateShell'
import { useDataLoader } from '../hooks/useDataLoader'
import { useToast } from '../hooks/useToast'
import { DEFAULT_PAGE_SIZE_OPTIONS, usePersistedPageSize } from '../hooks/usePersistedPageSize'
import { getTimeRangeISO, type TimeRangeKey } from '../lib/timeRange'
import { opsErrorLogAgentFilters, usageLogIdFromEvidence } from '../lib/logAgent'
import { formatCompactEmail } from '../lib/utils'
import { errorKindBadgeClassName, errorKindLabel, errorStatusBadgeClassName } from '../lib/errorBadges'
import { formatBeijingTime } from '../utils/time'
import type { APIKeyRow, OpsErrorAccountGroup, OpsErrorSummary, UsageLog } from '../types'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'

const ERROR_TIME_RANGES: TimeRangeKey[] = ['1h', '6h', '24h', '7d', '30d']
const pageSizeOptions = DEFAULT_PAGE_SIZE_OPTIONS

const errorTableHeadClass = 'text-[12px] font-semibold'
const errorTableTextClass = 'text-[14px]'
const errorTableMonoClass = 'font-geist-mono text-[13px] tabular-nums'

// Errors list views: one row per error, or one row per account.
type ErrorsView = 'individual' | 'account'
const ACCOUNT_ERROR_ROWS = 10

// Quick filters behind the stat tiles; a tile toggles its filter and clears
// the other tile filters.
type QuickFilter = 'status5xx' | 'status401' | 'status429' | 'timeout' | 'retry'
const QUICK_FILTER_STATUS: Partial<Record<QuickFilter, string>> = { status5xx: '5xx', status401: '401', status429: '429' }

// activeQuickFilter reports which stat tile the current filters match.
function activeQuickFilter(status: string, timeout: boolean, retry: string): QuickFilter | null {
  if (timeout) return 'timeout'
  if (retry === 'true') return 'retry'
  const entry = Object.entries(QUICK_FILTER_STATUS).find(([, value]) => value === status)
  return entry ? (entry[0] as QuickFilter) : null
}

// `transport` scopes every query to one usage-log transport (a plugin page
// embeds this view); `embedded` drops the Ops page header and tabs.
export default function OperationsErrors({ transport, embedded = false }: { transport?: string; embedded?: boolean } = {}) {
  const { t } = useTranslation()
  const { toast, showToast } = useToast()
  const [timeRange, setTimeRange] = useState<TimeRangeKey>(embedded ? '24h' : '1h')
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = usePersistedPageSize('ops_errors', 20, pageSizeOptions)
  const [statusFilter, setStatusFilter] = useState('')
  const [errorKindFilter, setErrorKindFilter] = useState('')
  const [endpointFilter, setEndpointFilter] = useState('')
  const [apiKeyFilter, setApiKeyFilter] = useState('')
  const [streamFilter, setStreamFilter] = useState<'' | 'true' | 'false'>('')
  const [accountFilter, setAccountFilter] = useState('')
  const [retryFilter, setRetryFilter] = useState('')
  const [timeoutFilter, setTimeoutFilter] = useState(false)
  const [view, setView] = useState<ErrorsView>('individual')
  const [accountPage, setAccountPage] = useState(1)
  const [accountPageSize, setAccountPageSize] = usePersistedPageSize('ops_errors_accounts', 20, pageSizeOptions)
  const [expandedAccount, setExpandedAccount] = useState<number | null>(null)
  const [searchInput, setSearchInput] = useState('')
  const [searchQuery, setSearchQuery] = useState('')
  const [exportDedupe, setExportDedupe] = useState(true)
  const [exportExcludeAuthRateLimit, setExportExcludeAuthRateLimit] = useState(true)
  const [exporting, setExporting] = useState(false)
  const [selectedLog, setSelectedLog] = useState<UsageLog | null>(null)
  const searchTimer = useRef<ReturnType<typeof setTimeout>>(null)

  const handleSearchChange = useCallback((value: string) => {
    setSearchInput(value)
    if (searchTimer.current) {
      clearTimeout(searchTimer.current)
    }
    searchTimer.current = setTimeout(() => {
      setSearchQuery(value.trim())
      setPage(1)
    }, 400)
  }, [])

  useEffect(() => () => {
    if (searchTimer.current) {
      clearTimeout(searchTimer.current)
    }
  }, [])

  const buildBaseParams = useCallback(() => {
    const range = getTimeRangeISO(timeRange)
    return {
      start: range.start,
      end: range.end,
      status: statusFilter,
      errorKind: errorKindFilter,
      endpoint: endpointFilter,
      apiKeyId: apiKeyFilter,
      stream: streamFilter,
      q: searchQuery,
      transport,
      accountId: accountFilter,
      retry: retryFilter,
      timeout: timeoutFilter ? 'true' : '',
    }
  }, [accountFilter, apiKeyFilter, endpointFilter, errorKindFilter, retryFilter, searchQuery, statusFilter, streamFilter, timeRange, timeoutFilter, transport])

  const loadErrorData = useCallback(async () => {
    const baseParams = buildBaseParams()
    // The per-account groups ignore the account filter so the account
    // picker keeps listing every account with errors.
    const [summary, pageResult, apiKeysResult, byAccount] = await Promise.all([
      api.getOpsErrorSummary(baseParams),
      api.getOpsErrors({
        ...baseParams,
        page,
        pageSize,
      }),
      api.getAPIKeys().catch(() => ({ keys: [] as APIKeyRow[] })),
      api.getOpsErrorsByAccount({ ...baseParams, accountId: '' }).catch(() => ({ accounts: [] as OpsErrorAccountGroup[] })),
    ])

    return {
      summary,
      logs: pageResult.logs ?? [],
      total: pageResult.total ?? 0,
      apiKeys: apiKeysResult.keys ?? [],
      accountGroups: byAccount.accounts ?? [],
    }
  }, [buildBaseParams, page, pageSize])

  const { data, loading, error, reload, reloadSilently } = useDataLoader<{
    summary: OpsErrorSummary | null
    logs: UsageLog[]
    total: number
    apiKeys: APIKeyRow[]
    accountGroups: OpsErrorAccountGroup[]
  }>({
    initialData: {
      summary: null,
      logs: [],
      total: 0,
      apiKeys: [],
      accountGroups: [],
    },
    load: loadErrorData,
  })

  useEffect(() => {
    const timer = window.setInterval(() => {
      void reloadSilently()
    }, 15000)

    return () => window.clearInterval(timer)
  }, [reloadSilently])

  const hasActiveFilters = Boolean(statusFilter || errorKindFilter || endpointFilter || apiKeyFilter || streamFilter || searchQuery || accountFilter || retryFilter || timeoutFilter)
  const quickFilter = activeQuickFilter(statusFilter, timeoutFilter, retryFilter)
  const toggleQuickFilter = (next: QuickFilter) => {
    const clear = quickFilter === next
    setStatusFilter(clear ? '' : QUICK_FILTER_STATUS[next] ?? '')
    setTimeoutFilter(!clear && next === 'timeout')
    setRetryFilter(!clear && next === 'retry' ? 'true' : '')
    setPage(1)
    setAccountPage(1)
  }
  const accountGroups = accountFilter
    ? data.accountGroups.filter((group) => String(group.account_id) === accountFilter)
    : data.accountGroups
  const accountTotalPages = Math.max(1, Math.ceil(accountGroups.length / accountPageSize))
  const currentAccountPage = Math.min(accountPage, accountTotalPages)
  const visibleAccountGroups = accountGroups.slice((currentAccountPage - 1) * accountPageSize, currentAccountPage * accountPageSize)
  const accountOptions = useMemo(() => {
    const options = [{ label: t('opsErrors.allAccounts'), value: '' }]
    for (const group of data.accountGroups) {
      options.push({ label: `${formatAccountLabel(group)} · ${group.total}`, value: String(group.account_id) })
    }
    if (accountFilter && !data.accountGroups.some((group) => String(group.account_id) === accountFilter)) {
      options.push({ label: `ID ${accountFilter}`, value: accountFilter })
    }
    return options
  }, [accountFilter, data.accountGroups, t])
  const totalPages = Math.max(1, Math.ceil(data.total / pageSize))
  const currentPage = Math.min(page, totalPages)
  const apiKeyOptions = useMemo(() => [
    { label: t('opsErrors.allApiKeys'), value: '' },
    ...data.apiKeys.map((apiKey) => ({
      label: apiKey.name ? `${apiKey.name} · ${apiKey.key}` : apiKey.key,
      value: String(apiKey.id),
    })),
  ], [data.apiKeys, t])

  useEffect(() => {
    if (page > totalPages) {
      setPage(totalPages)
    }
  }, [page, totalPages])

  const resetFilters = () => {
    setStatusFilter('')
    setErrorKindFilter('')
    setEndpointFilter('')
    setApiKeyFilter('')
    setStreamFilter('')
    setSearchInput('')
    setSearchQuery('')
    setAccountFilter('')
    setRetryFilter('')
    setTimeoutFilter(false)
    setPage(1)
    setAccountPage(1)
  }

  const filterToAccount = (accountId: number) => {
    setAccountFilter(String(accountId))
    setView('individual')
    setPage(1)
  }

  const handleExport = async () => {
    setExporting(true)
    try {
      const blob = await api.downloadOpsErrors({
        ...buildBaseParams(),
        dedupe: exportDedupe,
        excludeStatus: exportExcludeAuthRateLimit ? '401,429' : '',
      })
      downloadBlob(blob, buildOpsErrorExportFilename(timeRange, exportDedupe, exportExcludeAuthRateLimit))
      showToast(t('opsErrors.exportSuccess'))
    } catch (err) {
      showToast(err instanceof Error ? err.message : t('opsErrors.exportFailed'), 'error')
    } finally {
      setExporting(false)
    }
  }

  // 分析结论引用的证据若在当前页，直接打开详情；否则提示调整筛选。
  const openEvidence = (evidenceId: string) => {
    const id = usageLogIdFromEvidence(evidenceId)
    const log = id === null ? undefined : data.logs.find((item) => item.id === id)
    if (log) {
      setSelectedLog(log)
    } else {
      showToast(t('logAgent.evidenceNotOnPage', { id: evidenceId }), 'info')
    }
  }

  const copyLog = async (log: UsageLog) => {
    const text = JSON.stringify({
      id: log.id,
      created_at: log.created_at,
      status_code: log.status_code,
      error_kind: log.upstream_error_kind,
      error_message: log.error_message,
      account_id: log.account_id,
      account_name: log.account_name,
      account_email: log.account_email,
      api_key_id: log.api_key_id,
      api_key_name: log.api_key_name,
      endpoint: log.inbound_endpoint || log.endpoint,
      upstream_endpoint: log.upstream_endpoint,
      model: log.model,
      effective_model: log.effective_model,
      stream: log.stream,
      duration_ms: log.duration_ms,
      first_token_ms: log.first_token_ms,
      is_retry_attempt: log.is_retry_attempt,
      attempt_index: log.attempt_index,
    }, null, 2)
    try {
      await copyTextToClipboard(text)
      showToast(t('opsErrors.copySuccess'))
    } catch {
      showToast(t('opsErrors.copyFailed'), 'error')
    }
  }

  return (
    <StateShell
      variant="page"
      loading={loading}
      error={error}
      onRetry={() => void reload()}
      loadingTitle={t('opsErrors.loadingTitle')}
      loadingDescription={t('opsErrors.loadingDesc')}
      errorTitle={t('opsErrors.errorTitle')}
    >
      <>
        {!embedded && (
          <PageHeader
            title={t('opsErrors.title')}
            description={t('opsErrors.description')}
            actions={
              <Button variant="outline" onClick={() => void reload()}>
                <RefreshCw className="size-3.5" />
                {t('common.refresh')}
              </Button>
            }
          />
        )}
        {!embedded && <OpsTabs />}

        <div className="mb-6 grid grid-cols-2 gap-2.5 sm:gap-4 lg:grid-cols-3 xl:grid-cols-6">
          <StatTile
            label={t('opsErrors.totalErrors')}
            value={formatNumber(data.summary?.total_errors ?? 0)}
            icon={<AlertCircle className="size-4" />}
            tone="danger"
            active={!hasActiveFilters}
            onClick={resetFilters}
          />
          <StatTile
            label="5xx"
            value={formatNumber(data.summary?.status_5xx ?? 0)}
            icon={<ServerCrash className="size-4" />}
            tone="danger"
            active={quickFilter === 'status5xx'}
            onClick={() => toggleQuickFilter('status5xx')}
          />
          <StatTile
            label="401"
            value={formatNumber(data.summary?.unauthorized ?? 0)}
            icon={<ShieldAlert className="size-4" />}
            tone="danger"
            active={quickFilter === 'status401'}
            onClick={() => toggleQuickFilter('status401')}
          />
          <StatTile
            label="429"
            value={formatNumber(data.summary?.rate_limited ?? 0)}
            icon={<TimerReset className="size-4" />}
            tone="warning"
            active={quickFilter === 'status429'}
            onClick={() => toggleQuickFilter('status429')}
          />
          <StatTile
            label={t('opsErrors.timeouts')}
            value={formatNumber(data.summary?.timeouts ?? 0)}
            icon={<Clock3 className="size-4" />}
            tone="warning"
            active={quickFilter === 'timeout'}
            onClick={() => toggleQuickFilter('timeout')}
          />
          <StatTile
            label={t('opsErrors.retryAttempts')}
            value={formatNumber(data.summary?.retry_attempts ?? 0)}
            icon={<RotateCcw className="size-4" />}
            tone="info"
            active={quickFilter === 'retry'}
            onClick={() => toggleQuickFilter('retry')}
          />
        </div>

        <div className="mb-6">
          <LogAgentPanel
            source="ops_errors"
            filters={opsErrorLogAgentFilters(buildBaseParams())}
            getRange={() => {
              const { start, end } = buildBaseParams()
              return { start, end }
            }}
            description={t('opsErrors.logAgentDesc')}
            onEvidenceClick={openEvidence}
          />
        </div>

        <Card>
          <CardContent className="p-6">
            <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
              <div>
                <h3 className="text-base font-semibold text-foreground">{t('opsErrors.tableTitle')}</h3>
                <p className="mt-1 text-sm text-muted-foreground">{t('opsErrors.tableDesc')}</p>
              </div>
              <div className="flex flex-wrap items-center gap-2">
                <SegmentedTabs
                  size="sm"
                  tabs={[
                    { value: 'individual', label: t('opsErrors.viewIndividual') },
                    { value: 'account', label: t('opsErrors.viewByAccount') },
                  ]}
                  value={view}
                  onValueChange={(value) => { setView(value as ErrorsView); setExpandedAccount(null) }}
                />
                <SegmentedTabs
                  size="sm"
                  tabs={ERROR_TIME_RANGES.map((key) => ({
                    value: key,
                    label: t(`dashboard.timeRange${key.toUpperCase()}`),
                  }))}
                  value={timeRange}
                  onValueChange={(value) => { setTimeRange(value as TimeRangeKey); setPage(1); setAccountPage(1) }}
                />
              </div>
            </div>

            <div className="toolbar-surface mb-4 flex flex-wrap items-center gap-2">
              <div className="relative w-80 max-sm:w-full">
                <Search className="absolute left-2.5 top-1/2 -translate-y-1/2 size-3.5 text-muted-foreground pointer-events-none" />
                <Input
                  className="pl-8 h-8 rounded-lg text-[13px]"
                  placeholder={t('opsErrors.searchPlaceholder')}
                  value={searchInput}
                  onChange={(e: React.ChangeEvent<HTMLInputElement>) => handleSearchChange(e.target.value)}
                />
              </div>
              <Select
                className="w-36"
                compact
                value={statusFilter}
                onValueChange={(value) => { setStatusFilter(value); setPage(1) }}
                placeholder={t('opsErrors.allStatus')}
                options={[
                  { label: t('opsErrors.allStatus'), value: '' },
                  { label: '4xx', value: '4xx' },
                  { label: '5xx', value: '5xx' },
                  { label: '401', value: '401' },
                  { label: '403', value: '403' },
                  { label: '429', value: '429' },
                  { label: '499', value: '499' },
                ]}
              />
              <Select
                className="w-52"
                compact
                value={errorKindFilter}
                onValueChange={(value) => { setErrorKindFilter(value); setPage(1) }}
                placeholder={t('opsErrors.allErrorKinds')}
                options={[
                  { label: t('opsErrors.allErrorKinds'), value: '' },
                  { label: 'upstream_error', value: 'upstream_error' },
                  { label: 'upstream_timeout', value: 'upstream_timeout' },
                  { label: 'server_error', value: 'server_error' },
                  { label: 'rate_limit', value: 'rate_limit' },
                  { label: 'transport_error', value: 'transport_error' },
                ]}
              />
              <Select
                className="w-52"
                compact
                value={endpointFilter}
                onValueChange={(value) => { setEndpointFilter(value); setPage(1) }}
                placeholder={t('opsErrors.allEndpoints')}
                options={[
                  { label: t('opsErrors.allEndpoints'), value: '' },
                  { label: '/v1/responses', value: '/v1/responses' },
                  { label: '/v1/chat/completions', value: '/v1/chat/completions' },
                  { label: '/v1/messages', value: '/v1/messages' },
                  { label: '/v1/images/generations', value: '/v1/images/generations' },
                  { label: '/v1/images/edits', value: '/v1/images/edits' },
                ]}
              />
              <Select
                className="w-60"
                compact
                value={apiKeyFilter}
                onValueChange={(value) => { setApiKeyFilter(value); setPage(1) }}
                placeholder={t('opsErrors.allApiKeys')}
                options={apiKeyOptions}
              />
              <Select
                className="w-60"
                compact
                value={accountFilter}
                onValueChange={(value) => { setAccountFilter(value); setPage(1); setAccountPage(1) }}
                placeholder={t('opsErrors.allAccounts')}
                aria-label={t('opsErrors.accountFilter')}
                options={accountOptions}
              />
              <Select
                className="w-32"
                compact
                value={streamFilter}
                onValueChange={(value) => { setStreamFilter(value as '' | 'true' | 'false'); setPage(1) }}
                placeholder={t('usage.allTypes')}
                options={[
                  { label: t('usage.allTypes'), value: '' },
                  { label: 'Stream', value: 'true' },
                  { label: 'Sync', value: 'false' },
                ]}
              />
              <label className="inline-flex h-8 items-center gap-1.5 rounded-lg border border-border bg-background px-2.5 text-[13px] text-muted-foreground">
                <input
                  type="checkbox"
                  checked={exportDedupe}
                  onChange={(event) => setExportDedupe(event.target.checked)}
                  className="size-3.5 rounded border-border"
                />
                {t('opsErrors.exportDedupe')}
              </label>
              <label className="inline-flex h-8 items-center gap-1.5 rounded-lg border border-border bg-background px-2.5 text-[13px] text-muted-foreground">
                <input
                  type="checkbox"
                  checked={exportExcludeAuthRateLimit}
                  onChange={(event) => setExportExcludeAuthRateLimit(event.target.checked)}
                  className="size-3.5 rounded border-border"
                />
                {t('opsErrors.exclude401429')}
              </label>
              <Button variant="outline" size="sm" onClick={() => void handleExport()} disabled={exporting}>
                <Download className="size-3.5" />
                {exporting ? t('opsErrors.exporting') : t('opsErrors.exportJson')}
              </Button>
              {hasActiveFilters && (
                <button
                  type="button"
                  onClick={resetFilters}
                  className="h-8 px-2.5 rounded-lg border border-border bg-background text-[13px] text-muted-foreground hover:text-foreground hover:bg-muted/50 transition-colors inline-flex items-center gap-1"
                >
                  <X className="size-3.5" />
                  {t('usage.clearFilters')}
                </button>
              )}
              <span className="ml-auto text-xs text-muted-foreground max-sm:ml-0">
                {view === 'account' ? t('opsErrors.accountsCount', { count: accountGroups.length }) : t('usage.recordsCount', { count: data.total })}
              </span>
            </div>

            {view === 'account' ? (
              <StateShell
                variant="section"
                isEmpty={accountGroups.length === 0}
                emptyTitle={t('opsErrors.emptyTitle')}
                emptyDescription={hasActiveFilters ? t('opsErrors.emptyFilteredDesc') : t('opsErrors.emptyDesc')}
              >
                <p className="mb-3 text-xs leading-relaxed text-muted-foreground">{t('opsErrors.byAccountHint')}</p>
                <AccountErrorGroups
                  groups={visibleAccountGroups}
                  expanded={expandedAccount}
                  onToggle={(id) => setExpandedAccount((current) => (current === id ? null : id))}
                  onFilter={filterToAccount}
                  onOpenLog={setSelectedLog}
                  baseParams={buildBaseParams}
                />
                <Pagination
                  page={currentAccountPage}
                  totalPages={accountTotalPages}
                  onPageChange={setAccountPage}
                  totalItems={accountGroups.length}
                  pageSize={accountPageSize}
                  pageSizeOptions={pageSizeOptions}
                  onPageSizeChange={(nextPageSize) => {
                    setAccountPageSize(nextPageSize)
                    setAccountPage(1)
                  }}
                />
              </StateShell>
            ) : (
            <StateShell
              variant="section"
              isEmpty={data.logs.length === 0}
              emptyTitle={t('opsErrors.emptyTitle')}
              emptyDescription={hasActiveFilters ? t('opsErrors.emptyFilteredDesc') : t('opsErrors.emptyDesc')}
            >
              <div className="grid grid-cols-1 gap-3 lg:hidden">
                {data.logs.map((log) => (
                  <Card key={log.id} className="min-w-0 p-3.5 space-y-2.5">
                    <div className="flex items-start justify-between gap-2">
                      <div className="flex flex-wrap items-center gap-1.5 min-w-0">
                        <Badge variant="outline" className={`text-[12px] ${errorStatusBadgeClassName(log.status_code)}`}>
                          {log.status_code}
                        </Badge>
                        <Badge variant="outline" className={`text-[12px] ${errorKindBadgeClassName(errorKindLabel(log))}`}>
                          {errorKindLabel(log)}
                        </Badge>
                        <Badge variant="outline" className="text-[12px] truncate max-w-[140px]">{log.model || '-'}</Badge>
                      </div>
                      <span className="shrink-0 font-geist-mono text-[11px] text-muted-foreground">
                        {formatDuration(log.duration_ms)}
                      </span>
                    </div>

                    <div className="text-xs space-y-1">
                      <div className="font-geist-mono text-muted-foreground truncate" title={formatEndpoint(log)}>
                        {formatEndpoint(log)}
                      </div>
                      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-muted-foreground">
                        <span title={formatAccountTitle(log)}>{formatAccountLabel(log)}</span>
                        {formatAPIKeyLabel(log) ? (
                          <>
                            <span>·</span>
                            <span title={formatAPIKeyLabel(log)}>{formatAPIKeyLabel(log)}</span>
                          </>
                        ) : null}
                      </div>
                    </div>

                    {log.error_message ? (
                      <div className="rounded-lg bg-destructive/5 border border-destructive/20 p-2 text-xs text-destructive line-clamp-3 break-words leading-relaxed">
                        {log.error_message}
                      </div>
                    ) : null}

                    <div className="flex items-center justify-between pt-1 border-t border-border/50 text-[11px] text-muted-foreground">
                      <span>{formatBeijingTime(log.created_at)}</span>
                      <Button variant="outline" size="xs" onClick={() => setSelectedLog(log)}>
                        {t('opsErrors.details')}
                      </Button>
                    </div>
                  </Card>
                ))}
              </div>

              <div className="data-table-shell hidden lg:block">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead className={errorTableHeadClass}>{t('usage.tableTime')}</TableHead>
                      <TableHead className={errorTableHeadClass}>{t('usage.tableStatus')}</TableHead>
                      <TableHead className={errorTableHeadClass}>{t('opsErrors.errorKind')}</TableHead>
                      <TableHead className={errorTableHeadClass}>{t('usage.tableModel')}</TableHead>
                      <TableHead className={errorTableHeadClass}>{t('usage.tableEndpoint')}</TableHead>
                      <TableHead className={errorTableHeadClass}>{t('usage.tableAccount')}</TableHead>
                      <TableHead className={errorTableHeadClass}>{t('usage.tableApiKey')}</TableHead>
                      <TableHead className={errorTableHeadClass}>{t('usage.tableDuration')}</TableHead>
                      <TableHead className={errorTableHeadClass}>{t('opsErrors.errorSummary')}</TableHead>
                      <TableHead className={errorTableHeadClass}>{t('common.actions')}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {data.logs.map((log) => (
                      <TableRow key={log.id}>
                        <TableCell className={`${errorTableMonoClass} text-muted-foreground`}>{formatBeijingTime(log.created_at)}</TableCell>
                        <TableCell>
                          <Badge variant="outline" className={`text-[13px] ${errorStatusBadgeClassName(log.status_code)}`}>
                            {log.status_code}
                          </Badge>
                        </TableCell>
                        <TableCell>
                          <Badge variant="outline" className={errorKindBadgeClassName(errorKindLabel(log))}>
                            {errorKindLabel(log)}
                          </Badge>
                        </TableCell>
                        <TableCell>
                          <div className="flex flex-wrap items-center gap-1.5">
                            <Badge variant="outline" className="text-[13px]">{log.model || '-'}</Badge>
                            {log.effective_model && log.effective_model !== log.model && (
                              <Badge variant="outline" className="border-transparent bg-blue-500/10 text-blue-600 dark:bg-blue-500/20 dark:text-blue-400">
                                {log.effective_model}
                              </Badge>
                            )}
                          </div>
                        </TableCell>
                        <TableCell>
                          <div className={`${errorTableMonoClass} max-w-[240px] truncate text-muted-foreground`} title={formatEndpoint(log)}>
                            {formatEndpoint(log)}
                          </div>
                        </TableCell>
                        <TableCell className={`${errorTableTextClass} text-muted-foreground`}>
                          <span className="block max-w-[180px] truncate whitespace-nowrap" title={formatAccountTitle(log)}>
                            {formatAccountLabel(log)}
                          </span>
                        </TableCell>
                        <TableCell className={`${errorTableTextClass} text-muted-foreground`}>
                          <span className="block max-w-[160px] truncate" title={formatAPIKeyLabel(log)}>
                            {formatAPIKeyLabel(log) || t('usage.unknownApiKey')}
                          </span>
                        </TableCell>
                        <TableCell>
                          <span className={`${errorTableMonoClass} ${log.duration_ms > 30000 ? 'text-red-500' : log.duration_ms > 10000 ? 'text-amber-500' : 'text-muted-foreground'}`}>
                            {formatDuration(log.duration_ms)}
                          </span>
                        </TableCell>
                        <TableCell className="max-w-[360px] whitespace-normal">
                          <div className="line-clamp-2 text-[13px] leading-relaxed text-muted-foreground" title={log.error_message || ''}>
                            {log.error_message || t('opsErrors.noErrorMessage')}
                          </div>
                        </TableCell>
                        <TableCell>
                          <Button variant="outline" size="xs" onClick={() => setSelectedLog(log)}>
                            {t('opsErrors.details')}
                          </Button>
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </div>
              <Pagination
                page={currentPage}
                totalPages={totalPages}
                onPageChange={setPage}
                totalItems={data.total}
                pageSize={pageSize}
                pageSizeOptions={pageSizeOptions}
                onPageSizeChange={(nextPageSize) => {
                  setPageSize(nextPageSize)
                  setPage(1)
                }}
              />
            </StateShell>
            )}
          </CardContent>
        </Card>

        <Dialog open={Boolean(selectedLog)} onOpenChange={(open) => { if (!open) setSelectedLog(null) }}>
          {selectedLog ? (
            <DialogContent className="max-h-[86vh] overflow-y-auto sm:max-w-4xl">
              <DialogHeader>
                <DialogTitle>{t('opsErrors.detailTitle', { id: selectedLog.id })}</DialogTitle>
                <DialogDescription>{formatBeijingTime(selectedLog.created_at)}</DialogDescription>
              </DialogHeader>
              <div className="flex flex-wrap items-center gap-2">
                <Badge variant="outline" className={`text-[13px] ${errorStatusBadgeClassName(selectedLog.status_code)}`}>
                  HTTP {selectedLog.status_code}
                </Badge>
                <Badge variant="outline" className={errorKindBadgeClassName(errorKindLabel(selectedLog))}>{errorKindLabel(selectedLog)}</Badge>
                {selectedLog.is_retry_attempt && (
                  <Badge variant="outline" className="border-transparent bg-blue-500/10 text-blue-600 dark:bg-blue-500/20 dark:text-blue-400">
                    {t('opsErrors.retryAttempt', { index: selectedLog.attempt_index })}
                  </Badge>
                )}
              </div>

              <div className="rounded-lg border border-border bg-muted/30 p-4">
                <div className="mb-2 text-[12px] font-semibold uppercase text-muted-foreground">{t('opsErrors.fullError')}</div>
                <pre className="max-h-56 overflow-auto whitespace-pre-wrap break-words font-geist-mono text-[12px] leading-relaxed text-foreground">
                  {selectedLog.error_message || t('opsErrors.noErrorMessage')}
                </pre>
              </div>

              <div className="grid gap-4 md:grid-cols-2">
                <DetailPanel title={t('opsErrors.requestContext')}>
                  <DetailRow label={t('usage.tableEndpoint')} value={formatEndpoint(selectedLog)} mono />
                  <DetailRow label={t('usage.tableModel')} value={selectedLog.effective_model && selectedLog.effective_model !== selectedLog.model ? `${selectedLog.model} → ${selectedLog.effective_model}` : selectedLog.model || '-'} />
                  <DetailRow label={t('usage.tableType')} value={selectedLog.stream ? 'stream' : 'sync'} />
                  <DetailRow label="Service Tier" value={selectedLog.service_tier || '-'} />
                  <DetailRow label="Reasoning" value={selectedLog.reasoning_effort || '-'} />
                </DetailPanel>
                <DetailPanel title={t('opsErrors.runtimeContext')}>
                  <DetailRow label={t('usage.tableAccount')} value={formatAccountDetail(selectedLog)} />
                  <DetailRow label={t('usage.tableApiKey')} value={formatAPIKeyLabel(selectedLog) || t('usage.unknownApiKey')} />
                  <DetailRow label={t('usage.tableDuration')} value={formatDuration(selectedLog.duration_ms)} mono />
                  <DetailRow label={t('usage.tableFirstToken')} value={selectedLog.first_token_ms > 0 ? formatDuration(selectedLog.first_token_ms) : '-'} mono />
                  <DetailRow label="Tokens" value={`${selectedLog.input_tokens} / ${selectedLog.output_tokens} / ${selectedLog.reasoning_tokens}`} mono />
                  <DetailRow label={t('usage.injectedTurnState')} value={selectedLog.injected_turn_state || '-'} mono />
                  <DetailRow label={t('usage.upstreamTurnState')} value={selectedLog.upstream_turn_state || '-'} mono />
                </DetailPanel>
              </div>

              {selectedLog.request_id ? (
                <div className="rounded-lg border border-border bg-card/75 p-4">
                  <LogAgentPanel
                    key={selectedLog.request_id}
                    bare
                    showHistory={false}
                    source="usage_logs"
                    refs={[selectedLog.request_id]}
                    title={t('opsErrors.logAgentRequestTitle')}
                    description={t('opsErrors.logAgentRequestDesc')}
                  />
                </div>
              ) : null}

              <div className="flex justify-end gap-2">
                <Button variant="outline" onClick={() => void copyLog(selectedLog)}>
                  <Copy className="size-3.5" />
                  {t('opsErrors.copyJson')}
                </Button>
              </div>
            </DialogContent>
          ) : null}
        </Dialog>
      </>
    </StateShell>
  )
}

function DetailPanel({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div className="rounded-lg border border-border bg-card/75 p-4">
      <div className="mb-3 text-[12px] font-semibold uppercase text-muted-foreground">{title}</div>
      <div className="space-y-2">{children}</div>
    </div>
  )
}

function DetailRow({ label, value, mono = false }: { label: string; value: string; mono?: boolean }) {
  return (
    <div className="grid grid-cols-[120px_minmax(0,1fr)] gap-3 text-sm">
      <span className="text-muted-foreground">{label}</span>
      <span className={`min-w-0 break-words text-foreground ${mono ? 'font-geist-mono text-[13px] tabular-nums' : ''}`}>{value}</span>
    </div>
  )
}

function formatNumber(value: number): string {
  return value.toLocaleString()
}

function formatDuration(value: number): string {
  if (!value || value <= 0) return '-'
  if (value >= 1000) return `${(value / 1000).toFixed(1)}s`
  return `${value}ms`
}

function formatAPIKeyLabel(log: UsageLog): string {
  const name = log.api_key_name?.trim()
  if (name) return name
  const masked = log.api_key_masked?.trim()
  if (!masked) return ''
  if (masked.length <= 8) return masked
  return `${masked.slice(0, 4)}...${masked.slice(-4)}`
}

type AccountLabelSource = Pick<UsageLog, 'account_id' | 'account_name' | 'account_email'>

function formatAccountLabel(log: AccountLabelSource): string {
  // 邮箱优先：身份账号一律显示邮箱，账号名仅作为无邮箱账号（如 relay API-key 账号）的兜底。
  // 避免 AT 导入未命名时的占位名（at-account-N 等）盖过真实邮箱身份。
  const accountEmail = log.account_email?.trim()
  if (accountEmail) return formatCompactEmail(accountEmail)
  const accountName = log.account_name?.trim()
  if (accountName) return accountName
  return log.account_id > 0 ? `ID ${log.account_id}` : '-'
}

function formatAccountTitle(log: AccountLabelSource): string {
  const accountEmail = log.account_email?.trim()
  const accountName = log.account_name?.trim()
  if (accountEmail && accountName && accountEmail !== accountName) {
    return `${accountEmail} · ${accountName}`
  }
  return accountEmail || accountName || (log.account_id > 0 ? `ID ${log.account_id}` : '-')
}

function formatAccountDetail(log: UsageLog): string {
  const title = formatAccountTitle(log)
  if (log.account_id <= 0) return title
  return title === `ID ${log.account_id}` ? title : `${title} · ID ${log.account_id}`
}

function formatEndpoint(log: UsageLog): string {
  const inbound = log.inbound_endpoint || log.endpoint || '-'
  if (log.upstream_endpoint && log.upstream_endpoint !== inbound) {
    return `${inbound} → ${log.upstream_endpoint}`
  }
  return inbound
}

async function copyTextToClipboard(text: string) {
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text)
      return
    } catch {
      // Fall back for non-secure contexts or browsers that block clipboard writes.
    }
  }

  const textarea = document.createElement('textarea')
  textarea.value = text
  textarea.setAttribute('readonly', 'true')
  textarea.style.position = 'fixed'
  textarea.style.top = '-1000px'
  textarea.style.opacity = '0'
  textarea.style.pointerEvents = 'none'
  document.body.appendChild(textarea)
  textarea.select()
  textarea.setSelectionRange(0, text.length)
  const copied = document.execCommand('copy')
  document.body.removeChild(textarea)
  if (!copied) {
    throw new Error('copy failed')
  }
}

function downloadBlob(blob: Blob, filename: string) {
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  document.body.appendChild(a)
  a.click()
  document.body.removeChild(a)
  URL.revokeObjectURL(url)
}

function buildOpsErrorExportFilename(timeRange: TimeRangeKey, dedupe: boolean, excludeAuthRateLimit: boolean) {
  const timestamp = new Date().toISOString().replace(/[:.]/g, '-')
  const options = [
    dedupe ? 'deduped' : 'raw',
    excludeAuthRateLimit ? 'no-401-429' : 'all-status',
  ].join('-')
  return `ops-errors-${timeRange}-${options}-${timestamp}.json`
}

// AccountErrorGroups is the per-account errors view: per account the total,
// a colored count per error kind and the latest error; expanding a row loads
// that account's most recent errors under the same filters.
function AccountErrorGroups({ groups, expanded, onToggle, onFilter, onOpenLog, baseParams }: {
  groups: OpsErrorAccountGroup[]
  expanded: number | null
  onToggle: (accountId: number) => void
  onFilter: (accountId: number) => void
  onOpenLog: (log: UsageLog) => void
  baseParams: () => Parameters<typeof api.getOpsErrorsByAccount>[0]
}) {
  const { t } = useTranslation()
  const sortedKinds = (group: OpsErrorAccountGroup) => Object.entries(group.kinds).sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
  const kindBadges = (group: OpsErrorAccountGroup) => sortedKinds(group).map(([kind, count]) => (
    <Badge key={kind} variant="outline" className={`gap-1 text-[12px] ${errorKindBadgeClassName(kind)}`}>
      {kind}
      <span className="font-geist-mono tabular-nums opacity-80">×{count}</span>
    </Badge>
  ))
  return (
    <>
    <div className="grid grid-cols-1 gap-3 lg:hidden">
      {groups.map((group) => {
        const open = expanded === group.account_id
        return (
          <Card key={group.account_id} className="min-w-0 space-y-2.5 p-3.5">
            <div className="flex items-start justify-between gap-2">
              <span className="min-w-0 truncate text-sm font-medium" title={formatAccountTitle(group)}>{formatAccountLabel(group)}</span>
              <span className="shrink-0 font-geist-mono text-sm font-semibold tabular-nums">{formatNumber(group.total)}</span>
            </div>
            <div className="flex flex-wrap gap-1">{kindBadges(group)}</div>
            <div className="flex flex-wrap items-center justify-between gap-2 border-t border-border/50 pt-2 text-[11px] text-muted-foreground">
              <span>{t('opsErrors.lastError')} {formatBeijingTime(group.last_error_at)}</span>
              <div className="flex items-center gap-1.5">
                <Button variant="outline" size="xs" aria-expanded={open} onClick={() => onToggle(group.account_id)}>
                  {open ? t('opsErrors.collapse') : t('opsErrors.expand')}
                </Button>
                <Button variant="ghost" size="xs" onClick={() => onFilter(group.account_id)}>
                  <Filter className="size-3" />
                  {t('opsErrors.filterAccount')}
                </Button>
              </div>
            </div>
            {open && <AccountRecentErrors accountId={group.account_id} baseParams={baseParams} onOpenLog={onOpenLog} />}
          </Card>
        )
      })}
    </div>
    <div className="data-table-shell hidden lg:block">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead className={errorTableHeadClass}>{t('usage.tableAccount')}</TableHead>
            <TableHead className={`${errorTableHeadClass} text-right`}>{t('opsErrors.accountErrors')}</TableHead>
            <TableHead className={errorTableHeadClass}>{t('opsErrors.errorBreakdown')}</TableHead>
            <TableHead className={errorTableHeadClass}>{t('opsErrors.lastError')}</TableHead>
            <TableHead className={errorTableHeadClass}>{t('common.actions')}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {groups.map((group) => {
            const open = expanded === group.account_id
            return (
              <Fragment key={group.account_id}>
                <TableRow data-state={open ? 'selected' : undefined}>
                  <TableCell className={`${errorTableTextClass} max-w-[220px]`}>
                    <button
                      type="button"
                      className="flex min-w-0 items-center gap-1.5 text-left hover:text-primary"
                      aria-expanded={open}
                      onClick={() => onToggle(group.account_id)}
                    >
                      {open ? <ChevronDown className="size-3.5 shrink-0" /> : <ChevronRight className="size-3.5 shrink-0" />}
                      <span className="truncate" title={formatAccountTitle(group)}>{formatAccountLabel(group)}</span>
                    </button>
                  </TableCell>
                  <TableCell className={`${errorTableMonoClass} text-right font-semibold`}>{formatNumber(group.total)}</TableCell>
                  <TableCell className="min-w-[220px] whitespace-normal">
                    <div className="flex flex-wrap gap-1">{kindBadges(group)}</div>
                  </TableCell>
                  <TableCell className={`${errorTableMonoClass} whitespace-nowrap text-muted-foreground`}>{formatBeijingTime(group.last_error_at)}</TableCell>
                  <TableCell>
                    <div className="flex items-center gap-1.5">
                      <Button variant="outline" size="xs" onClick={() => onToggle(group.account_id)}>
                        {open ? t('opsErrors.collapse') : t('opsErrors.expand')}
                      </Button>
                      <Button variant="ghost" size="xs" onClick={() => onFilter(group.account_id)}>
                        <Filter className="size-3" />
                        {t('opsErrors.filterAccount')}
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
                {open && (
                  <TableRow className="hover:bg-transparent">
                    <TableCell colSpan={5} className="bg-muted/20 p-3">
                      <AccountRecentErrors accountId={group.account_id} baseParams={baseParams} onOpenLog={onOpenLog} />
                    </TableCell>
                  </TableRow>
                )}
              </Fragment>
            )
          })}
        </TableBody>
      </Table>
    </div>
    </>
  )
}

// AccountRecentErrors lists one account's latest errors under the filters.
function AccountRecentErrors({ accountId, baseParams, onOpenLog }: {
  accountId: number
  baseParams: () => Parameters<typeof api.getOpsErrorsByAccount>[0]
  onOpenLog: (log: UsageLog) => void
}) {
  const { t } = useTranslation()
  const load = useCallback(async () => {
    const res = await api.getOpsErrors({ ...baseParams(), accountId: String(accountId), page: 1, pageSize: ACCOUNT_ERROR_ROWS })
    return res.logs ?? []
  }, [accountId, baseParams])
  const { data: logs, loading, error } = useDataLoader<UsageLog[]>({ initialData: [], load })
  if (loading && logs.length === 0) return <p className="text-xs text-muted-foreground">{t('common.loading')}</p>
  if (error) return <p role="alert" className="text-xs text-destructive">{error}</p>
  return (
    <div className="space-y-1.5">
      <div className="text-[12px] font-semibold text-muted-foreground">{t('opsErrors.recentErrors', { count: logs.length })}</div>
      <ul className="divide-y divide-border rounded-lg border border-border bg-card">
        {logs.map((log) => (
          <li key={log.id} className="flex min-w-0 flex-wrap items-center gap-2 px-3 py-2 text-[13px]">
            <span className={`${errorTableMonoClass} text-muted-foreground`}>{formatBeijingTime(log.created_at)}</span>
            <Badge variant="outline" className={errorStatusBadgeClassName(log.status_code)}>{log.status_code}</Badge>
            <Badge variant="outline" className={errorKindBadgeClassName(errorKindLabel(log))}>{errorKindLabel(log)}</Badge>
            <span className="text-muted-foreground">{log.model || '-'}</span>
            <span className="min-w-0 basis-full truncate text-muted-foreground sm:basis-0 sm:flex-1" title={log.error_message || ''}>{log.error_message || t('opsErrors.noErrorMessage')}</span>
            <Button variant="outline" size="xs" onClick={() => onOpenLog(log)}>{t('opsErrors.details')}</Button>
          </li>
        ))}
      </ul>
    </div>
  )
}
