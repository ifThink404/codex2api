import { useCallback, useEffect, useRef, useState } from 'react'
import { AlertCircle, ChevronLeft, ChevronRight, Copy, Download, Search, ServerCrash, ShieldAlert, TimerReset } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '../api'
import OpsTabs from '../components/OpsTabs'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import { StatTile } from '../components/StatTile'
import { useDataLoader } from '../hooks/useDataLoader'
import { useToast } from '../hooks/useToast'
import { writeClipboardText } from '../lib/clipboard'
import { getTimeRangeISO, type TimeRangeKey } from '../lib/timeRange'
import { SERVICE_ERROR_STAGES, serviceErrorCollectorHasLoss, serviceErrorNewAPIUserLabel, type ServiceErrorEvent, type ServiceErrorPage, type ServiceErrorQuery } from '../lib/serviceErrors'
import { buildServiceErrorPageExport, saveServiceErrorPageExport } from '../lib/serviceErrorExport'
import { formatBeijingTime } from '../utils/time'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'

const emptyPage: ServiceErrorPage & { query?: ServiceErrorQuery; pageNumber?: number; snapshotKey?: string } = {
  items: [],
  summary: { total: 0, status_429: 0, status_4xx: 0, status_5xx: 0 },
  collector: { pending: 0, written: 0, dropped: 0, write_failures: 0, capacity: 512, retention_days: 7, max_rows: 100000 },
}

export default function ServiceErrors() {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const [filters, setFilters] = useState({ timeRange: '1h' as TimeRangeKey, status: '', stage: '', requestID: '', grouped: true, cursors: [''] })
  const [groupSelection, setGroupSelection] = useState<{ event: ServiceErrorEvent; query: ServiceErrorQuery } | null>(null)
  const [search, setSearch] = useState('')
  const range = useRef(getTimeRangeISO('1h'))
  const pending = useRef<AbortController | null>(null)
  const cursor = filters.cursors[filters.cursors.length - 1]
  const pageNumber = filters.cursors.length
  const snapshotKey = JSON.stringify([filters.timeRange, filters.status, filters.stage, filters.requestID, filters.grouped, cursor, pageNumber])
  const load = useCallback(async () => {
    pending.current?.abort()
    const controller = new AbortController()
    pending.current = controller
    if (!cursor) range.current = getTimeRangeISO(filters.timeRange)
    const query = { ...range.current, status: filters.status, stage: filters.stage, request_id: filters.requestID, grouped: filters.grouped, cursor }
    const page = await api.getServiceErrors(query, controller.signal)
    return { ...page, query, pageNumber, snapshotKey }
  }, [cursor, filters.requestID, filters.stage, filters.status, filters.timeRange, filters.grouped, pageNumber, snapshotKey])
  const { data, loading, error, reload, reloadSilently } = useDataLoader({ initialData: emptyPage, load })

  useEffect(() => () => pending.current?.abort(), [])
  useEffect(() => {
    if (cursor) return
    const timer = window.setInterval(() => {
      if (document.visibilityState === 'visible' && !loading) void reloadSilently()
    }, 30000)
    return () => window.clearInterval(timer)
  }, [cursor, loading, reloadSilently])

  const updateFilters = (change: Partial<Omit<typeof filters, 'cursors'>>) => {
    setFilters(current => ({ ...current, ...change, cursors: [''] }))
  }
  const canDownload = !loading && !error && data.items.length > 0 && !!data.query && data.snapshotKey === snapshotKey
  const downloadPage = () => {
    if (!canDownload || !data.query || !data.pageNumber) return
    try {
      saveServiceErrorPageExport(buildServiceErrorPageExport(data, data.query, data.pageNumber))
      showToast(t('serviceErrors.downloadSuccess', { count: data.items.length }))
    } catch {
      showToast(t('serviceErrors.downloadFailed'), 'error')
    }
  }

  return (
    <>
      <PageHeader title={t('serviceErrors.title')} description={t('serviceErrors.description')} actions={
        <Button type="button" variant="outline" size="sm" disabled={!canDownload} onClick={downloadPage} title={t('serviceErrors.downloadHint')}>
          <Download className="size-3.5" />{t('serviceErrors.downloadPage')}
        </Button>
      } onRefresh={() => {
        if (cursor) setFilters(current => ({ ...current, cursors: [''] }))
        else void reload()
      }} />
      <OpsTabs />
      <div className="mb-5 grid grid-cols-2 gap-3 xl:grid-cols-4">
        <StatTile label={t('serviceErrors.total')} value={data.summary.total.toLocaleString()} icon={<AlertCircle className="size-4" />} tone="danger" />
        <StatTile label="429" value={data.summary.status_429.toLocaleString()} icon={<TimerReset className="size-4" />} tone="warning" />
        <StatTile label="4xx" value={data.summary.status_4xx.toLocaleString()} icon={<ShieldAlert className="size-4" />} />
        <StatTile label="5xx" value={data.summary.status_5xx.toLocaleString()} icon={<ServerCrash className="size-4" />} tone="danger" />
      </div>
      <Card className="mb-4">
        <CardContent className="space-y-3 p-4">
          <div className="flex flex-wrap items-center gap-2">
            <div className="flex items-center gap-1" aria-label={t('serviceErrors.timeRange')}>
              {(['1h', '6h', '24h', '7d'] as const).map(value => (
                <Button key={value} size="sm" variant={filters.timeRange === value ? 'secondary' : 'ghost'} aria-pressed={filters.timeRange === value} onClick={() => updateFilters({ timeRange: value })}>{value}</Button>
              ))}
            </div>
            <Select value={filters.status} onValueChange={status => updateFilters({ status })} options={[
              { value: '', label: t('serviceErrors.allStatuses') },
              ...['429', '4xx', '5xx'].map(value => ({ value, label: value })),
            ]} className="w-32" />
            <Select value={filters.stage} onValueChange={stage => updateFilters({ stage })} options={[
              { value: '', label: t('serviceErrors.allStages') },
              ...SERVICE_ERROR_STAGES.map(value => ({ value, label: t(`serviceErrors.stages.${value}`) })),
            ]} className="w-40" />
            <div className="flex items-center gap-1 rounded-lg border p-1" role="group" aria-label={t('serviceErrors.view')}>
              {[true, false].map(grouped => <Button key={String(grouped)} type="button" size="sm" variant={filters.grouped === grouped ? 'secondary' : 'ghost'} aria-pressed={filters.grouped === grouped} onClick={() => updateFilters({ grouped })}>{t(grouped ? 'serviceErrors.grouped' : 'serviceErrors.individual')}</Button>)}
            </div>
            <form className="flex min-w-0 flex-1 gap-2 max-sm:basis-full" onSubmit={event => { event.preventDefault(); updateFilters({ requestID: search.trim() }) }}>
              <Input value={search} onChange={event => setSearch(event.target.value)} placeholder={t('serviceErrors.searchPlaceholder')} aria-label={t('serviceErrors.searchPlaceholder')} maxLength={160} className="min-w-0" />
              <Button type="submit" variant="outline" aria-label={t('serviceErrors.search')}><Search className="size-4" /></Button>
            </form>
          </div>
          {filters.grouped && <p className="text-xs leading-relaxed text-muted-foreground">{t('serviceErrors.groupHint')}</p>}
          {filters.grouped && (data.summary.grouping_pending ?? 0) > 0 && <p className="text-xs text-amber-600 dark:text-amber-400" role="status">{t('serviceErrors.groupingPending', { count: data.summary.grouping_pending })}</p>}
          <p className="text-xs leading-relaxed text-muted-foreground">{t('serviceErrors.retention', { days: data.collector.retention_days, rows: data.collector.max_rows.toLocaleString() })}</p>
          <p className={`text-xs ${serviceErrorCollectorHasLoss(data.collector) ? 'text-amber-600 dark:text-amber-400' : 'text-muted-foreground'}`} role={serviceErrorCollectorHasLoss(data.collector) ? 'status' : undefined}>
            {t('serviceErrors.collector', { pending: data.collector.pending, dropped: data.collector.dropped, failed: data.collector.write_failures })}
            {serviceErrorCollectorHasLoss(data.collector) ? ` · ${t('serviceErrors.lossWarning')}` : ''}
          </p>
        </CardContent>
      </Card>
      <StateShell loading={loading} error={error} onRetry={() => void reload()}>
        <ServiceErrorResults items={data.items} grouped={data.grouped} onGroup={event => {
          if (event.group && data.query) setGroupSelection({ event, query: { ...data.query, grouped: false, group_key: event.group.key, cursor: '' } })
        }} />
      </StateShell>
      <div className="mt-4 flex items-center justify-between gap-3 text-sm text-muted-foreground">
        <span>{t('serviceErrors.page', { page: filters.cursors.length })}{data.grouped ? ` · ${t('serviceErrors.groupSummary', { groups: data.summary.groups ?? 0, total: data.summary.total })}` : ''}</span>
        <div className="flex gap-2">
          <Button variant="outline" size="sm" disabled={!cursor || loading} onClick={() => setFilters(current => ({ ...current, cursors: current.cursors.slice(0, -1) }))}><ChevronLeft className="size-4" />{t('common.prev')}</Button>
          <Button variant="outline" size="sm" disabled={!data.next_cursor || loading || !!error} onClick={() => {
            if (data.next_cursor) setFilters(current => ({ ...current, cursors: [...current.cursors, data.next_cursor!] }))
          }}>{t('common.next')}<ChevronRight className="size-4" /></Button>
        </div>
      </div>
      {groupSelection && <ServiceErrorGroupDialog event={groupSelection.event} query={groupSelection.query} onClose={() => setGroupSelection(null)} />}
    </>
  )
}

export function ServiceErrorResults({ items, grouped = false, onGroup }: { items: ServiceErrorEvent[]; grouped?: boolean; onGroup?: (event: ServiceErrorEvent) => void }) {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const [selected, setSelected] = useState<ServiceErrorEvent | null>(null)
  const copyContainer = useRef<HTMLDivElement>(null)
  const copy = async () => {
    if (!selected) return
    try {
      await writeClipboardText(JSON.stringify(selected, null, 2), copyContainer.current)
      showToast(t('opsErrors.copySuccess'))
    } catch {
      showToast(t('opsErrors.copyFailed'), 'error')
    }
  }

  return (
    <>
      <Card className="overflow-hidden">
        {items.length === 0 ? (
          <div className="flex flex-col items-center gap-2 px-5 py-14 text-center">
            <ShieldAlert className="mb-1 size-7 text-muted-foreground" />
            <p className="font-medium">{t('serviceErrors.empty')}</p>
            <p className="max-w-lg text-sm text-muted-foreground">{t('serviceErrors.emptyDescription')}</p>
          </div>
        ) : (
          <Table className="min-w-[900px]">
            <TableHeader><TableRow>
              {['time', ...(grouped ? ['count'] : []), 'status', 'stage', 'request', 'identity', 'error'].map(key => <TableHead key={key}>{t(`serviceErrors.columns.${key}`)}</TableHead>)}
              <TableHead className="w-20"><span className="sr-only">{t('opsErrors.details')}</span></TableHead>
            </TableRow></TableHeader>
            <TableBody>{items.map(item => {
              const newAPIUser = serviceErrorNewAPIUserLabel(item)
              return (
                <TableRow key={item.id}>
                  <TableCell className="whitespace-nowrap text-xs">
                    <div>{item.group ? `${t('serviceErrors.lastSeen')} · ` : ''}<span className="font-geist-mono">{formatBeijingTime(item.group?.last_seen || item.created_at)}</span></div>
                    <div className="mt-1 text-muted-foreground">{item.group ? `${t('serviceErrors.firstSeen')} · ${formatBeijingTime(item.group.first_seen)}` : `${item.duration_ms.toLocaleString()} ms`}</div>
                  </TableCell>
                  {grouped && <TableCell><Button type="button" size="sm" variant="outline" disabled={!item.group || !onGroup} onClick={() => onGroup?.(item)} aria-label={t('serviceErrors.viewOccurrences', { count: item.group?.count ?? 1 })}>{t('serviceErrors.occurrences', { count: item.group?.count ?? 1 })}</Button></TableCell>}
                  <TableCell><Badge variant={item.status_code >= 500 ? 'destructive' : 'outline'}>{item.status_code}</Badge><div className="mt-1 text-xs text-muted-foreground">{item.transport.toUpperCase()}</div></TableCell>
                  <TableCell className="whitespace-nowrap text-sm">{t(`serviceErrors.stages.${item.stage}`, { defaultValue: item.stage })}</TableCell>
                  <TableCell className="max-w-52 text-xs"><div className="truncate font-medium" title={item.model}>{item.model || '—'}</div><div className="mt-1 truncate text-muted-foreground" title={item.endpoint}>{item.method} {item.endpoint}</div><div className="mt-1 truncate text-muted-foreground" title={item.thread_source || item.request_type}>{item.thread_source || item.request_type}</div></TableCell>
                  <TableCell className="max-w-44 text-xs">
                    <div className="truncate" title={item.api_key_name}>{item.api_key_name || (item.api_key_id ? `Key #${item.api_key_id}` : t('serviceErrors.unidentified'))}</div>
                    {newAPIUser && <div className="mt-1 truncate font-medium" title={`NewAPI · ${newAPIUser}`}>NewAPI · {newAPIUser}</div>}
                    <div className="mt-1 truncate font-geist-mono text-muted-foreground" title={item.request_id}>{item.request_id}</div>
                  </TableCell>
                  <TableCell className="max-w-80"><div className="truncate font-geist-mono text-xs" title={item.code}>{item.code}</div><p className="mt-1 line-clamp-2 whitespace-normal break-words text-sm text-muted-foreground">{item.message}</p></TableCell>
                  <TableCell><Button type="button" size="sm" variant="ghost" onClick={() => setSelected(item)}>{t(grouped ? 'serviceErrors.latestDetails' : 'opsErrors.details')}</Button></TableCell>
                </TableRow>
              )
            })}</TableBody>
          </Table>
        )}
      </Card>
      <Dialog open={selected !== null} onOpenChange={open => { if (!open) setSelected(null) }}>
        <DialogContent ref={copyContainer} className="sm:max-w-3xl">
          <DialogHeader><DialogTitle>{t('serviceErrors.details')}</DialogTitle><DialogDescription>{t('serviceErrors.detailsDescription')}</DialogDescription></DialogHeader>
          <div className="flex items-start justify-between gap-4"><p className="min-w-0 break-words text-sm">{selected?.message}</p><Button type="button" variant="outline" size="sm" onClick={() => void copy()}><Copy className="size-3.5" />{t('serviceErrors.copy')}</Button></div>
          <pre className="max-h-[55vh] overflow-auto whitespace-pre-wrap break-all rounded-lg border bg-muted/40 p-4 font-geist-mono text-xs leading-relaxed">{JSON.stringify(selected, null, 2)}</pre>
        </DialogContent>
      </Dialog>
    </>
  )
}

function ServiceErrorGroupDialog({ event, query, onClose }: { event: ServiceErrorEvent; query: ServiceErrorQuery; onClose: () => void }) {
  const { t } = useTranslation()
  const [cursors, setCursors] = useState([''])
  const pending = useRef<AbortController | null>(null)
  const cursor = cursors[cursors.length - 1]
  const load = useCallback(() => {
    pending.current?.abort()
    const controller = new AbortController()
    pending.current = controller
    return api.getServiceErrors({ ...query, cursor }, controller.signal)
  }, [query, cursor])
  const { data, loading, error, reload } = useDataLoader<ServiceErrorPage>({ initialData: emptyPage, load })
  useEffect(() => () => pending.current?.abort(), [])
  return (
    <Dialog open onOpenChange={open => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-6xl">
        <DialogHeader>
          <DialogTitle>{t('serviceErrors.groupDetails')}</DialogTitle>
          <DialogDescription>{t('serviceErrors.groupDetailsDescription')}</DialogDescription>
        </DialogHeader>
        <p className="break-words text-sm">{event.model} · {event.endpoint} · {event.code}</p>
        <div className="max-h-[55vh] overflow-auto"><StateShell loading={loading} error={error} onRetry={() => void reload()}><ServiceErrorResults items={data.items} /></StateShell></div>
        <div className="flex items-center justify-between gap-3 text-sm text-muted-foreground">
          <span>{t('serviceErrors.occurrences', { count: data.summary.total })} · {t('serviceErrors.page', { page: cursors.length })}</span>
          <div className="flex gap-2">
            <Button variant="outline" size="sm" disabled={!cursor || loading} onClick={() => setCursors(value => value.slice(0, -1))}><ChevronLeft className="size-4" />{t('common.prev')}</Button>
            <Button variant="outline" size="sm" disabled={!data.next_cursor || loading || !!error} onClick={() => { if (data.next_cursor) setCursors(value => [...value, data.next_cursor!]) }}>{t('common.next')}<ChevronRight className="size-4" /></Button>
          </div>
        </div>
      </DialogContent>
    </Dialog>
  )
}
