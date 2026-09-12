import { useEffect, useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { useSearchParams } from 'react-router-dom'
import { Bike, Check, Code2, Copy, Download, Eye, FlaskConical, RefreshCw, Play, RotateCcw, Square, History, Clock3, ArrowUpRight, X, ExternalLink, Loader2, FileImage } from 'lucide-react'
import { api } from '../api'
import type { AccountRow, UpstreamChannel } from '../types'
import PageHeader from '../components/PageHeader'
import { SegmentedPillGroup } from '../components/ui/segmented-pill-group'
import { Button } from '../components/ui/button'
import { Input } from '../components/ui/input'
import { Select } from '../components/ui/select'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '../components/ui/dialog'
import { useToast } from '../hooks/useToast'
import { useVisibleChannels } from '../visibleChannels'
import { formatAccountName } from '../lib/connectionTestModels'
import { extractQualityTestHTML, extractQualityTestSVG, PELICAN_PROMPT, qualityTestPreviewDocument, isQualityTestActive, qualityTestPlanTone, type QualityTestJob } from '../lib/qualityTest'
import { getErrorMessage } from '../utils/error'
import { useQualityTestJobs, useQualityTestDetail } from '../hooks/useQualityTestJobs'
import { useHighlightedHtml } from '../hooks/useHighlighter'
import { formatBeijingTime } from '../utils/time'
import Pagination from '../components/Pagination'
import './quality-test.css'

function PlanBadge({ plan }: { plan: string }) {
  return <span className={`quality-test-plan quality-test-plan--${qualityTestPlanTone(plan)}`}>{plan || '—'}</span>
}

function AccountChoice({ account, compact = false }: { account: AccountRow; compact?: boolean }) {
  return <span className={`quality-test-account-option ${compact ? 'is-compact' : ''}`}>
    <span className="quality-test-account-identity"><span>{formatAccountName(account)}</span>{!compact ? <small>#{account.id}{account.name && account.email && account.name !== account.email ? ` · ${account.email}` : ''}</small> : null}</span>
    <PlanBadge plan={account.plan_type} />
  </span>
}

// 超过该体积的输出只显示纯文本,避免高亮阻塞主线程。
const HIGHLIGHT_LIMIT = 200 * 1024

function SourceView({ source }: { source: string }) {
  const { t } = useTranslation()
  const highlighted = useHighlightedHtml(source && source.length <= HIGHLIGHT_LIMIT ? source : '', 'html')
  if (highlighted) return <div className="quality-test-source shiki-wrapper" tabIndex={0} aria-label={t('qualityTest.source')} dangerouslySetInnerHTML={{ __html: highlighted }} />
  return <pre className="quality-test-source" tabIndex={0} aria-label={t('qualityTest.source')}><code>{source || t('qualityTest.sourceEmpty')}</code></pre>
}

const formatSeconds = (ms?: number) => ms === undefined ? '—' : `${(ms / 1000).toFixed(1)} s`

function downloadQualityFile(content: string, type: string, extension: 'html' | 'svg', id?: number) {
  const url = URL.createObjectURL(new Blob([content], { type }))
  const link = document.createElement('a')
  link.href = url
  link.download = `pelican-${id ?? Date.now()}.${extension}`
  link.click()
  window.setTimeout(() => URL.revokeObjectURL(url), 1000)
}

// 结果画布:工作台与检测记录弹窗共用,预览 iframe 走隔离页 + postMessage 注入。
function ResultCanvas({ run, view, preview, previewKey, narrow, loading }: { run: QualityTestJob | null; view: 'preview' | 'source'; preview: string; previewKey: number; narrow: boolean; loading?: boolean }) {
  const { t } = useTranslation()
  const running = isQualityTestActive(run)
  return <div className={`quality-test-canvas ${view === 'source' ? 'is-source' : ''}`}>
    {loading && !run ? <div className="quality-test-empty"><Loader2 className="size-6 animate-spin text-muted-foreground" /></div> :
      view === 'source' ? <SourceView source={run?.output ?? ''} /> : preview ?
      <iframe key={`${run?.id}-${previewKey}`} title={t('qualityTest.previewTitle')} src="/api/quality-test/preview" sandbox="allow-scripts" referrerPolicy="no-referrer" onLoad={(event) => event.currentTarget.contentWindow?.postMessage({ type: 'quality-test-preview', html: preview }, '*')} className={narrow ? 'is-narrow' : ''} /> :
      <div className="quality-test-empty">
        <div className="quality-test-illustration"><Bike strokeWidth={1.2} className="size-20" /><span>SVG</span></div>
        <span className="quality-test-tag">PELICAN BENCH</span>
        <h4>{t(running ? 'qualityTest.generating' : run ? 'qualityTest.noHTML' : 'qualityTest.emptyTitle')}</h4>
        <p>{t(running ? 'qualityTest.generatingHint' : run ? 'qualityTest.noHTMLHint' : 'qualityTest.emptyHint')}</p>
        {running ? <div className="quality-test-progress"><span /></div> : null}
      </div>}
  </div>
}

function PreviewActions({ run, html, view, narrow, onToggleNarrow, onReplay, onCopy }: { run: QualityTestJob | null; html: string; view: 'preview' | 'source'; narrow: boolean; onToggleNarrow: () => void; onReplay: () => void; onCopy: () => void }) {
  const { t } = useTranslation()
  const svg = useMemo(() => html ? extractQualityTestSVG(html) : '', [html])
  return <div>
    <Button size="sm" variant="ghost" disabled={!html || view !== 'preview'} aria-pressed={narrow} onClick={onToggleNarrow}>{narrow ? '100%' : '390px'}</Button>
    <Button size="icon-sm" variant="ghost" title={t('qualityTest.replay')} aria-label={t('qualityTest.replay')} disabled={!html} onClick={onReplay}><RotateCcw /></Button>
    <Button size="icon-sm" variant="ghost" title={t('qualityTest.copy')} aria-label={t('qualityTest.copy')} disabled={!run?.output} onClick={onCopy}><Copy /></Button>
    <Button size="icon-sm" variant="ghost" title={t('qualityTest.download')} aria-label={t('qualityTest.download')} disabled={!html} onClick={() => downloadQualityFile(html, 'text/html;charset=utf-8', 'html', run?.id)}><Download /></Button>
    <Button size="icon-sm" variant="ghost" title={t('qualityTest.downloadSVG')} aria-label={t('qualityTest.downloadSVG')} disabled={!svg} onClick={() => downloadQualityFile(svg, 'image/svg+xml;charset=utf-8', 'svg', run?.id)}><FileImage /></Button>
  </div>
}

function DetailMeta({ label, value, mono = true }: { label: string; value: string; mono?: boolean }) {
  return <div className="quality-test-dialog-meta"><dt>{label}</dt><dd className={mono ? 'is-mono' : ''}>{value}</dd></div>
}

// 检测记录的结果弹窗:全屏拟态窗口,左侧渲染动画,右侧列出账号/模型/耗时等详情。
function ResultDialog({ id, revision, onClose, onOpenStudio }: { id: number | undefined; revision: number; onClose: () => void; onOpenStudio: (id: number) => void }) {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const detail = useQualityTestDetail(id, revision)
  const run = detail.job
  const [view, setView] = useState<'preview' | 'source'>('preview')
  const [previewKey, setPreviewKey] = useState(0)
  const [narrow, setNarrow] = useState(false)
  const html = useMemo(() => run && !isQualityTestActive(run) ? extractQualityTestHTML(run.output ?? '') : '', [run])
  const preview = useMemo(() => html ? qualityTestPreviewDocument(html) : '', [html])
  useEffect(() => { setView('preview'); setNarrow(false); setPreviewKey(0) }, [id])
  const running = isQualityTestActive(run)

  async function copySource() {
    try {
      await navigator.clipboard.writeText(html || run?.output || '')
      showToast(t('qualityTest.copied'))
    } catch { showToast(t('qualityTest.copyFailed'), 'error') }
  }

  return <Dialog open={Boolean(id)} onOpenChange={(open) => { if (!open) onClose() }}>
    <DialogContent className="quality-test-dialog !flex !h-[calc(100dvh-1.5rem)] !w-[min(1480px,calc(100vw-1.5rem))] !max-w-none flex-col gap-0 overflow-hidden p-0 sm:p-0" showCloseButton={false}>
      <DialogHeader className="quality-test-dialog-header">
        <div className="quality-test-dialog-heading">
          <DialogTitle className="text-sm">{t('qualityTest.result')}{id ? <span className="quality-test-dialog-id">#{id}</span> : null}</DialogTitle>
          <DialogDescription className="truncate text-[11px]">{run ? `${run.account_name} · #${run.account_id} / ${run.model} / ${run.reasoning_effort || t('qualityTest.efforts.default')}` : t('qualityTest.loadingRecords')}</DialogDescription>
        </div>
        <SegmentedPillGroup value={view} onChange={setView} label={t('qualityTest.result')} options={[{ value: 'preview', label: t('qualityTest.preview'), icon: <Eye className="size-3.5" /> }, { value: 'source', label: t('qualityTest.source'), icon: <Code2 className="size-3.5" /> }]} />
      </DialogHeader>
      <Button type="button" size="icon-sm" variant="ghost" onClick={onClose} className="absolute right-3 top-3 z-10" aria-label={t('common.close')}><X className="size-4" /></Button>
      <div className="quality-test-dialog-layout">
        <div className="quality-test-dialog-stage">
          {detail.error ? <div role="alert" className="quality-test-error quality-test-result-error">{detail.error}</div> : null}
          {run?.error ? <div role="alert" className="quality-test-error quality-test-result-error">{run.error}</div> : null}
          <ResultCanvas run={run} view={view} preview={preview} previewKey={previewKey} narrow={narrow} loading={detail.loading} />
        </div>
        <aside className="quality-test-dialog-info">
          <div className="quality-test-dialog-status">
            <span className={`quality-test-status ${run?.status ?? ''}`} role="status">{running ? <RefreshCw className="size-3.5 animate-spin" /> : run?.status === 'completed' ? <Check className="size-3.5" /> : <span className="quality-test-status-dot" />}{t(`qualityTest.status.${run?.status ?? 'idle'}`)}</span>
            {run ? <PlanBadge plan={run.plan_type} /> : null}
          </div>
          <h3>{t('qualityTest.runDetails')}</h3>
          <dl className="quality-test-dialog-grid">
            <DetailMeta label={t('qualityTest.account')} value={run ? `${run.account_name} · #${run.account_id}` : '—'} mono={false} />
            <DetailMeta label={t('qualityTest.model')} value={run?.model ?? '—'} />
            <DetailMeta label={t('qualityTest.effort')} value={run ? run.reasoning_effort || t('qualityTest.efforts.default') : '—'} />
            <DetailMeta label={t('qualityTest.testTime')} value={run ? formatBeijingTime(run.created_at) : '—'} />
            <DetailMeta label={t('qualityTest.duration')} value={run ? formatSeconds(run.duration_ms) : '—'} />
            <DetailMeta label={t('qualityTest.firstContent')} value={formatSeconds(run?.first_content_ms)} />
            <DetailMeta label={t('qualityTest.outputTokens')} value={run?.output_tokens?.toLocaleString() ?? '—'} />
            <DetailMeta label={t('qualityTest.reasoningTokens')} value={run?.reasoning_tokens?.toLocaleString() ?? '—'} />
          </dl>
          {run?.response_model ? <p className="quality-test-response-model !p-0 mt-3">{t('qualityTest.responseModel')}: {run.response_model}</p> : null}
          {run?.prompt ? <><div className="mt-6 flex items-center justify-between gap-2"><h3 className="!mb-0">{t('qualityTest.savedPrompt')}</h3></div><p className="quality-test-dialog-prompt">{run.prompt}</p>{run.completed_at ? <small className="quality-test-dialog-finished">{t('qualityTest.finishedAt')}: {formatBeijingTime(run.completed_at)}</small> : null}</> : null}
        </aside>
      </div>
      <div className="quality-test-dialog-footer">
        <div className="quality-test-preview-actions !border-0 !p-0">
          <span className="quality-test-hint">{t('qualityTest.isolatedPreview')}</span>
          <PreviewActions run={run} html={html} view={view} narrow={narrow} onToggleNarrow={() => setNarrow((value) => !value)} onReplay={() => setPreviewKey((key) => key + 1)} onCopy={() => void copySource()} />
        </div>
        <Button size="sm" variant="outline" disabled={!id} onClick={() => id && onOpenStudio(id)}><ExternalLink className="size-3.5" />{t('qualityTest.openInStudio')}</Button>
      </div>
    </DialogContent>
  </Dialog>
}

type TestOptions = { models: string[]; reasoning_efforts: string[] }
const channelNames: Record<UpstreamChannel, string> = { codex: 'Codex / Responses', claude: 'Claude', grok: 'Grok', antigravity: 'Antigravity' }

export default function QualityTest() {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const { channels } = useVisibleChannels()
  const [channel, setChannel] = useState<UpstreamChannel>('codex')
  const [search, setSearch] = useState('')
  const [accounts, setAccounts] = useState<AccountRow[]>([])
  const [account, setAccount] = useState<AccountRow | null>(null)
  const [accountLoading, setAccountLoading] = useState(true)
  const [total, setTotal] = useState(0)
  const [options, setOptions] = useState<TestOptions | null>(null)
  const [optionsLoading, setOptionsLoading] = useState(false)
  const [loadError, setLoadError] = useState('')
  const [reload, setReload] = useState(0)
  const [model, setModel] = useState('')
  const [effort, setEffort] = useState('high')
  const [prompt, setPrompt] = useState(PELICAN_PROMPT)
  const [searchParams, setSearchParams] = useSearchParams()
  const pane = searchParams.get('view') === 'history' ? 'history' : 'studio'
  const [recordPage, setRecordPage] = useState(1)
  const [modalID, setModalID] = useState<number>()
  const [revision, setRevision] = useState(0)
  const records = useQualityTestJobs(recordPage, revision)
  const requestedID = Number(searchParams.get('job'))
  const selectedID = requestedID > 0 ? requestedID : records.active_jobs[0]?.id ?? records.jobs[0]?.id
  const detail = useQualityTestDetail(pane === 'studio' ? selectedID : undefined, revision)
  const openRecord = (id: number) => setModalID(id)
  const run = detail.job
  const [submitting, setSubmitting] = useState(false)
  const [cancelling, setCancelling] = useState(false)
  const submittingRef = useRef(false)
  const [view, setView] = useState<'preview' | 'source'>('preview')
  const [previewKey, setPreviewKey] = useState(0)
  const [narrowPreview, setNarrowPreview] = useState(false)
  const mountedRef = useRef(true)
  const running = isQualityTestActive(run)
  const slotsFull = records.active_jobs.length >= records.concurrency_limit
  const accountBusy = records.active_jobs.some((job) => job.account_id === account?.id)
  const promptBytes = new TextEncoder().encode(prompt).length
  const html = useMemo(() => run && !isQualityTestActive(run) ? extractQualityTestHTML(run.output ?? '') : '', [run])
  const preview = useMemo(() => html ? qualityTestPreviewDocument(html) : '', [html])
  const accountChoices = account && !accounts.some((row) => row.id === account.id) ? [account, ...accounts] : accounts
  const shownChannels = [...new Set<UpstreamChannel>(['codex', ...channels])]

  useEffect(() => {
    mountedRef.current = true
    return () => { mountedRef.current = false }
  }, [])

  useEffect(() => {
    const controller = new AbortController()
    setAccountLoading(true)
    const timer = window.setTimeout(() => {
      api.getAccountsPage({ channel, page: 1, pageSize: 100, search }, controller.signal)
        .then((result) => {
          if (controller.signal.aborted) return
          setAccounts(result.accounts)
          setTotal(result.total)
          setLoadError('')
        })
        .catch((error) => { if (!controller.signal.aborted) setLoadError(getErrorMessage(error)) })
        .finally(() => { if (!controller.signal.aborted) setAccountLoading(false) })
    }, 250)
    return () => { window.clearTimeout(timer); controller.abort() }
  }, [channel, search, reload])

  const accountID = account?.id
  useEffect(() => {
    setOptions(null)
    setModel('')
    if (!accountID) { setOptionsLoading(false); return }
    const controller = new AbortController()
    setOptionsLoading(true)
    api.getQualityTestOptions(accountID, controller.signal)
      .then((result) => {
        if (controller.signal.aborted) return
        setOptions(result)
        setModel(result.models[0] ?? '')
        setEffort((current) => result.reasoning_efforts.includes(current) ? current : '')
        setLoadError('')
      })
      .catch((error) => { if (!controller.signal.aborted) setLoadError(getErrorMessage(error)) })
      .finally(() => { if (!controller.signal.aborted) setOptionsLoading(false) })
    return () => controller.abort()
  }, [accountID, reload])

  function selectJob(id: number, nextPane = pane) {
    setSearchParams({ view: nextPane, job: String(id) })
    setView('preview')
    setPreviewKey((key) => key + 1)
  }

  async function startTest() {
    if (submittingRef.current || !account || !model || !prompt.trim() || promptBytes > 16000 || slotsFull || accountBusy) return
    submittingRef.current = true
    setSubmitting(true)
    try {
      const result = await api.createQualityTest(account.id, { model, reasoning_effort: effort, prompt })
      if (!mountedRef.current) return
      selectJob(result.job.id, 'studio')
      setRecordPage(1)
      setRevision((value) => value + 1)
      showToast(t('qualityTest.backgroundStarted'))
    } catch (error) {
      if (mountedRef.current) { showToast(getErrorMessage(error), 'error'); setRevision((value) => value + 1) }
    } finally {
      submittingRef.current = false
      if (mountedRef.current) setSubmitting(false)
    }
  }

  async function stopTest() {
    if (!run || !running || cancelling) return
    setCancelling(true)
    try {
      await api.cancelQualityTest(run.id)
      if (mountedRef.current) setRevision((value) => value + 1)
    } catch (error) { if (mountedRef.current) showToast(getErrorMessage(error), 'error') }
    finally { if (mountedRef.current) setCancelling(false) }
  }

  async function copySource() {
    try {
      await navigator.clipboard.writeText(html || run?.output || '')
      showToast(t('qualityTest.copied'))
    } catch { showToast(t('qualityTest.copyFailed'), 'error') }
  }

  const formatTime = formatSeconds
  return (
    <div className="quality-test-page">
      <PageHeader title={t('qualityTest.title')} description={t('qualityTest.subtitle')}
        titleAdornment={<span className="quality-test-tag"><FlaskConical className="size-3.5" /> HTML / SVG</span>}
        actionMeta={<span className="quality-test-capacity"><span className={records.active_jobs.length ? 'is-active' : ''} />{t('qualityTest.capacity', { count: records.active_jobs.length, limit: records.concurrency_limit })}</span>}
        actions={<SegmentedPillGroup value={pane} label={t('qualityTest.title')} onChange={(value) => setSearchParams((previous) => { const next = new URLSearchParams(previous); next.set('view', value); return next })}
          options={[{ value: 'studio', label: t('qualityTest.studioTab'), icon: <FlaskConical className="size-4" /> }, { value: 'history', label: t('qualityTest.recordsTab'), icon: <History className="size-4" /> }]} />} />
      {records.error || detail.error ? <div role="alert" className="quality-test-error">{records.error || detail.error}<Button size="sm" variant="outline" onClick={() => setRevision((value) => value + 1)}>{t('common.retry')}</Button></div> : null}
      {records.active_jobs.length > 0 ? <section className="quality-test-active" aria-label={t('qualityTest.activeTasks')}>
        <div className="quality-test-active-heading"><span><RefreshCw className="size-3.5 animate-spin" />{t('qualityTest.activeTasks')}</span><p>{t('qualityTest.backgroundHint')}</p></div>
        <div className="quality-test-active-list">{records.active_jobs.map((job) => <Button variant="outline" key={job.id} className="quality-test-active-card" aria-pressed={selectedID === job.id} onClick={() => selectJob(job.id, 'studio')}>
          <span className="quality-test-active-account"><span>{job.account_name}</span><PlanBadge plan={job.plan_type} /></span>
          <span className="quality-test-active-model">{job.model} · {job.reasoning_effort || t('qualityTest.efforts.default')}</span>
          <span className="quality-test-active-meta">#{job.id} · {t(`qualityTest.status.${job.status}`)}<span>{formatTime(job.duration_ms)}<ArrowUpRight className="size-3.5" /></span></span>
        </Button>)}</div>
      </section> : null}
      {pane === 'history' ? <section className="quality-test-records" aria-labelledby="quality-records-title">
        <div className="quality-test-records-heading">
          <div><h3 id="quality-records-title">{t('qualityTest.recordsTab')}</h3><p>{t('qualityTest.recordsHint')}</p></div>
          <div className="quality-test-records-tools">{records.total > 0 ? <span className="quality-test-records-count">{records.total}</span> : null}<Button variant="outline" size="sm" onClick={() => setRevision((value) => value + 1)}><RefreshCw className={records.loading ? 'animate-spin' : ''} />{t('common.refresh')}</Button></div>
        </div>
        {records.jobs.length === 0 ? <div className="quality-test-records-empty"><History className="size-9" /><h4>{t(records.loading ? 'qualityTest.loadingRecords' : 'qualityTest.emptyRecords')}</h4><p>{t('qualityTest.emptyRecordsHint')}</p></div> : <div className="quality-test-records-scroll"><table>
          <thead><tr><th>{t('qualityTest.recordID')}</th><th>{t('qualityTest.account')}</th><th>{t('qualityTest.model')}</th><th>{t('qualityTest.effort')}</th><th>{t('qualityTest.testTime')}</th><th>{t('qualityTest.recordStatus')}</th><th className="is-numeric">{t('qualityTest.duration')}</th><th className="is-numeric">{t('qualityTest.firstContent')}</th><th className="is-numeric">{t('qualityTest.outputTokens')}</th><th><span className="sr-only">{t('qualityTest.viewResult')}</span></th></tr></thead>
          <tbody>{records.jobs.map((job) => <tr key={job.id} className={modalID === job.id ? 'is-selected' : ''} onClick={() => openRecord(job.id)}>
            <td className="quality-test-record-id">#{job.id}</td>
            <td><div className="quality-test-record-account"><span title={job.account_name}>{job.account_name}</span><small>#{job.account_id}<PlanBadge plan={job.plan_type} /></small></div></td>
            <td><span className="quality-test-record-model">{job.model}</span></td><td><span className="quality-test-effort-chip">{job.reasoning_effort || t('qualityTest.efforts.default')}</span></td>
            <td className="quality-test-record-time">{formatBeijingTime(job.created_at)}</td>
            <td><span className={`quality-test-status quality-test-status-pill ${job.status}`}>{isQualityTestActive(job) ? <RefreshCw className="size-3 animate-spin" /> : <span className="quality-test-status-dot" />}{t(`qualityTest.status.${job.status}`)}</span></td>
            <td className="quality-test-record-time is-numeric">{formatTime(job.duration_ms)}</td>
            <td className="quality-test-record-time is-numeric">{formatTime(job.first_content_ms)}</td>
            <td className="quality-test-record-time is-numeric">{job.output_tokens?.toLocaleString() ?? '—'}</td>
            <td className="quality-test-record-action"><Button size="sm" variant="outline" onClick={(event) => { event.stopPropagation(); openRecord(job.id) }}><Eye className="size-3.5" />{t('qualityTest.viewResult')}</Button></td>
          </tr>)}</tbody>
        </table></div>}
        <Pagination page={recordPage} totalPages={Math.ceil(records.total / 20)} onPageChange={setRecordPage} totalItems={records.total} pageSize={20} />
      </section> : <>
      <div className="quality-test-workspace">
        <section className="quality-test-controls" aria-labelledby="quality-config-title">
          <div className="quality-test-section-heading"><span className="quality-test-step">01</span><h3 id="quality-config-title">{t('qualityTest.configuration')}</h3></div>
          {loadError ? <div role="alert" className="quality-test-error">{loadError}<Button size="sm" variant="outline" onClick={() => setReload((value) => value + 1)}>{t('common.retry')}</Button></div> : null}
          <fieldset disabled={submitting} className="quality-test-fields">
            <label htmlFor="quality-channel">{t('qualityTest.channel')}</label>
            <Select id="quality-channel" value={channel} disabled={submitting} onValueChange={(value) => { setChannel(value as UpstreamChannel); setAccount(null); setAccounts([]); setSearch(''); setLoadError('') }} options={shownChannels.map((item) => ({ value: item, label: channelNames[item] }))} />
            <label htmlFor="quality-account-search">{t('qualityTest.account')}</label>
            <Input id="quality-account-search" value={search} onChange={(event) => setSearch(event.target.value)} placeholder={t('qualityTest.searchAccounts')} />
            <Select id="quality-account" aria-label={t('qualityTest.account')} value={String(account?.id ?? '')} disabled={accountLoading || submitting} placeholder={accountLoading ? t('qualityTest.loadingAccounts') : t('qualityTest.chooseAccount')} onValueChange={(value) => { setOptions(null); setModel(''); setAccount(accountChoices.find((row) => row.id === Number(value)) ?? null) }} options={accountChoices.map((row) => ({ value: String(row.id), label: `${formatAccountName(row)} · #${row.id} · ${row.plan_type || '—'}`, content: <AccountChoice account={row} />, triggerContent: <AccountChoice account={row} compact /> }))} />
            <p className="quality-test-hint">{!accountLoading && total === 0 ? t('qualityTest.noAccounts') : t('qualityTest.accountCount', { count: total })}</p>
            <label htmlFor="quality-model">{t('qualityTest.model')}</label>
            <Select id="quality-model" value={model} disabled={!options || optionsLoading || submitting} onValueChange={setModel} placeholder={optionsLoading ? t('qualityTest.loadingModels') : t('qualityTest.chooseModel')} options={(options?.models ?? []).map((item) => ({ value: item, label: item }))} />
            {options?.models.length === 0 ? <p className="quality-test-hint">{t('qualityTest.noModels')}</p> : null}
            <label htmlFor="quality-effort">{t('qualityTest.effort')}</label>
            <Select id="quality-effort" value={effort} disabled={!options || options.reasoning_efforts.length < 2 || submitting} onValueChange={setEffort} options={(options?.reasoning_efforts ?? ['high']).map((item) => ({ value: item, label: item ? t(`qualityTest.efforts.${item}`) : t('qualityTest.efforts.default') }))} />
            <p className="quality-test-hint">{t(options?.reasoning_efforts.length === 1 ? 'qualityTest.fixedEffort' : 'qualityTest.effortHint')}</p>
            <div className="quality-test-prompt-label"><label htmlFor="quality-prompt">{t('qualityTest.prompt')}</label><Button type="button" variant="ghost" size="xs" onClick={() => setPrompt(PELICAN_PROMPT)}>{t('qualityTest.resetPrompt')}</Button></div>
            <textarea id="quality-prompt" rows={5} value={prompt} onChange={(event) => setPrompt(event.target.value)} aria-invalid={promptBytes > 16000} />
            <p className="quality-test-hint">{t('qualityTest.promptHint')}</p>
            {promptBytes > 16000 ? <p role="alert" className="text-sm text-destructive">{t('qualityTest.promptTooLong')}</p> : null}
          </fieldset>
          <Button size="lg" className="w-full" onClick={() => void startTest()} disabled={submitting || !account || !model || optionsLoading || !prompt.trim() || promptBytes > 16000 || slotsFull || accountBusy}>
            {submitting ? <RefreshCw className="size-4 animate-spin" /> : <Play className="size-4" />}
            {t(submitting ? 'qualityTest.submitting' : accountBusy ? 'qualityTest.accountBusy' : slotsFull ? 'qualityTest.slotsFull' : 'qualityTest.start')}
          </Button>
          <p className="quality-test-hint quality-test-footnote">{t('qualityTest.runHint')}</p>
        </section>

        <section className="quality-test-results" aria-labelledby="quality-result-title">
          <div className="quality-test-result-toolbar">
            <div className="quality-test-section-heading"><span className="quality-test-step">02</span><h3 id="quality-result-title">{t('qualityTest.result')}</h3></div>
            <SegmentedPillGroup value={view} onChange={setView} label={t('qualityTest.result')} options={[{ value: 'preview', label: t('qualityTest.preview'), icon: <Eye className="size-3.5" /> }, { value: 'source', label: t('qualityTest.source'), icon: <Code2 className="size-3.5" /> }]} />
          </div>
          <div className="quality-test-run-meta">
            <span className={`quality-test-status ${run?.status ?? ''}`} role="status">{running ? <RefreshCw className="size-3.5 animate-spin" /> : run?.status === 'completed' ? <Check className="size-3.5" /> : <span className="quality-test-status-dot" />}{t(`qualityTest.status.${run?.status ?? 'idle'}`)}</span>
            <span title={run?.account_name}>{run ? `${run.account_name} · #${run.account_id} / ${run.model} / ${run.reasoning_effort || t('qualityTest.efforts.default')}` : t('qualityTest.readyHint')}</span>
          </div>
          {run ? <div className="quality-test-record-detail-meta"><span><Clock3 className="size-3.5" />{formatBeijingTime(run.created_at)}</span><PlanBadge plan={run.plan_type} /><span>#{run.id}</span>{running ? <Button size="sm" variant="outline" disabled={cancelling || run.status === 'cancelling'} onClick={() => void stopTest()}><Square className="size-3.5" />{t(run.status === 'cancelling' ? 'qualityTest.status.cancelling' : 'qualityTest.stop')}</Button> : null}</div> : null}
          {run?.error ? <div role="alert" className="quality-test-error quality-test-result-error">{run.error}</div> : null}
          <ResultCanvas run={run} view={view} preview={preview} previewKey={previewKey} narrow={narrowPreview} loading={detail.loading} />
          <div className="quality-test-preview-actions">
            <span className="quality-test-hint">{t('qualityTest.isolatedPreview')}</span>
            <PreviewActions run={run} html={html} view={view} narrow={narrowPreview} onToggleNarrow={() => setNarrowPreview((value) => !value)} onReplay={() => setPreviewKey((key) => key + 1)} onCopy={() => void copySource()} />
          </div>
          <dl className="quality-test-metrics">
            {[[t('qualityTest.duration'), run ? formatTime(run.duration_ms) : '—'], [t('qualityTest.firstContent'), formatTime(run?.first_content_ms)], [t('qualityTest.outputTokens'), run?.output_tokens?.toLocaleString() ?? '—'], [t('qualityTest.reasoningTokens'), run?.reasoning_tokens?.toLocaleString() ?? '—']].map(([label, value]) => <div key={label}><dt>{label}</dt><dd>{value}</dd></div>)}
          </dl>
          {run?.response_model ? <p className="quality-test-response-model">{t('qualityTest.responseModel')}: {run.response_model}</p> : null}
          {run?.prompt ? <details className="quality-test-saved-prompt"><summary>{t('qualityTest.savedPrompt')}</summary><p>{run.prompt}</p>{run.completed_at ? <small>{t('qualityTest.finishedAt')}: {formatBeijingTime(run.completed_at)}</small> : null}</details> : null}
        </section>
      </div>

      <section className="quality-test-review">
        <div><h3>{t('qualityTest.reviewTitle')}</h3><p>{t('qualityTest.reviewHint')}</p></div>
        <ul>{['shape', 'mechanics', 'motion', 'completion'].map((item, index) => <li key={item}><span>0{index + 1}</span>{t(`qualityTest.criteria.${item}`)}</li>)}</ul>
      </section>
      </>}
      <ResultDialog id={modalID} revision={revision} onClose={() => setModalID(undefined)} onOpenStudio={(id) => { setModalID(undefined); selectJob(id, 'studio') }} />
    </div>
  )
}
