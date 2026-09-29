import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react'
import { Link, Navigate, useParams } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import { Cable, ChevronRight, RefreshCw, Save, Search, Trash2 } from 'lucide-react'
import { api } from '../api'
import AccountGroupMultiSelect from '../components/AccountGroupMultiSelect'
import LogAgentPanel from '../components/LogAgentPanel'
import PageHeader from '../components/PageHeader'
import Pagination from '../components/Pagination'
import StateShell from '../components/StateShell'
import { SegmentedTabs } from '../components/SegmentedTabs'
import { StatTile } from '../components/StatTile'
import { useDataLoader } from '../hooks/useDataLoader'
import { useConfirmDialog } from '../hooks/useConfirmDialog'
import { useToast } from '../hooks/useToast'
import { isBPSAccount, type BPSTriState } from '../lib/bpsAccount'
import {
  PLUGIN_VIEWS,
  bpsConfigFields,
  CAPTURE_PURGE_MODES,
  formatCaptureBytes,
  normalizePluginConfig,
  pluginConfigBoolean,
  pluginConfigListText,
  pluginCoolingReasonKey,
  captureIdFromEvidence,
  pluginCaptureAgentFilters,
  pluginCaptureSource,
  normalizePluginView,
  parsePluginConfigText,
  pluginMetaSummary,
  sampleRateFromPercent,
  sampleRateToPercent,
  type PluginView,
} from '../lib/transportPlugins'
import { getTimeRangeISO, type TimeRangeKey } from '../lib/timeRange'
import { formatBeijingTime } from '../utils/time'
import { getErrorMessage } from '../utils/error'
import OperationsErrors from './OperationsErrors'
import type { AccountGroup, AccountRow, PluginAccountStatus, PluginCapture, PluginCapturePurgeMode, PluginCaptureStats, TransportPlugin, UsageLog } from '../types'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { DraftNumberInput } from '@/components/ui/draft-number-input'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'

const TIME_RANGES: TimeRangeKey[] = ['1h', '6h', '24h', '7d']
const monoClass = 'font-mono text-xs text-muted-foreground'

export default function Plugins() {
  const { id, view } = useParams()
  const { t } = useTranslation()
  // load must be stable: useDataLoader re-runs whenever it changes.
  const load = useCallback(async () => (await api.getTransportPlugins()).plugins ?? [], [])
  const { data, loading, error, reload } = useDataLoader<TransportPlugin[]>({ initialData: [], load })
  if (!id) {
    return (
      <StateShell variant="page" loading={loading} error={error} onRetry={() => void reload()} isEmpty={!loading && data.length === 0} emptyTitle={t('plugins.empty')}>
        <PageHeader title={t('plugins.title')} description={t('plugins.description')} onRefresh={() => void reload()} />
        <div className="grid gap-3 md:grid-cols-2">
          {data.map((plugin) => (
            <Link key={plugin.id} to={`/plugins/${plugin.id}/overview`} className="block">
              <Card className="transition-colors hover:border-primary/40">
                <CardContent className="flex items-start justify-between gap-3 p-4">
                  <div className="min-w-0 space-y-1">
                    <div className="flex items-center gap-2 text-sm font-semibold">
                      <Cable className="size-4 text-muted-foreground" aria-hidden />
                      {plugin.meta.name}
                      <Badge variant={plugin.state.enabled ? 'default' : 'secondary'}>
                        {plugin.state.enabled ? t('plugins.globalOn') : t('plugins.globalOff')}
                      </Badge>
                    </div>
                    <p className="text-xs text-muted-foreground">{t(`plugins.descriptions.${plugin.id}`, { defaultValue: plugin.meta.description })}</p>
                    <p className={monoClass}>{plugin.id}</p>
                  </div>
                  <ChevronRight className="size-4 shrink-0 text-muted-foreground" aria-hidden />
                </CardContent>
              </Card>
            </Link>
          ))}
        </div>
      </StateShell>
    )
  }
  const activeView = normalizePluginView(view)
  if (view !== activeView) return <Navigate to={`/plugins/${id}/${activeView}`} replace />
  const plugin = data.find((item) => item.id === id)
  return (
    <StateShell variant="page" loading={loading} error={error} onRetry={() => void reload()} isEmpty={!loading && !plugin} emptyTitle={t('plugins.notFound')}>
      {plugin && (
        <>
          <PageHeader
            title={plugin.meta.name}
            description={t(`plugins.descriptions.${plugin.id}`, { defaultValue: plugin.meta.description })}
            onRefresh={() => void reload()}
            titleAdornment={<span className={monoClass}>{plugin.id}</span>}
          />
          <div className="mb-5 flex justify-center">
            <SegmentedTabs
              className="w-full max-w-[760px]"
              tabs={PLUGIN_VIEWS.map((value) => ({ value, label: t(`plugins.views.${value}`), to: `/plugins/${plugin.id}/${value}` }))}
              value={activeView}
            />
          </div>
          <PluginViewBody plugin={plugin} view={activeView} onChanged={() => void reload()} />
        </>
      )}
    </StateShell>
  )
}

function PluginViewBody({ plugin, view, onChanged }: { plugin: TransportPlugin; view: PluginView; onChanged: () => void }) {
  switch (view) {
    case 'captures':
      return <PluginCaptures plugin={plugin} />
    case 'logs':
      return <PluginLogs plugin={plugin} />
    case 'errors':
      return <OperationsErrors transport={plugin.id} embedded />
    case 'agent':
      return <PluginAgent plugin={plugin} />
    default:
      return <PluginOverview plugin={plugin} onChanged={onChanged} />
  }
}

function Section({ title, description, actions, children }: { title: string; description?: string; actions?: ReactNode; children: ReactNode }) {
  return (
    <Card>
      <CardContent className="space-y-4 p-4 sm:p-5">
        <div className="flex flex-wrap items-start justify-between gap-2">
          <div className="min-w-0">
            <h2 className="text-sm font-semibold">{title}</h2>
            {description && <p className="mt-0.5 text-xs text-muted-foreground">{description}</p>}
          </div>
          {actions}
        </div>
        {children}
      </CardContent>
    </Card>
  )
}

function Field({ label, help, children }: { label: string; help?: string; children: ReactNode }) {
  return (
    <div className="space-y-1.5">
      <div className="text-xs font-medium">{label}</div>
      {children}
      {help && <p className="text-xs text-muted-foreground">{help}</p>}
    </div>
  )
}

function PluginOverview({ plugin, onChanged }: { plugin: TransportPlugin; onChanged: () => void }) {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const [saving, setSaving] = useState(false)
  const [groups, setGroups] = useState<AccountGroup[]>([])
  const [groupIds, setGroupIds] = useState<number[]>(plugin.state.group_ids ?? [])
  const [samplePercent, setSamplePercent] = useState(sampleRateToPercent(plugin.state.capture_sample_rate))
  const [config, setConfig] = useState<Record<string, unknown>>(plugin.state.config ?? {})
  const [configText, setConfigText] = useState(JSON.stringify(plugin.state.config ?? {}, null, 2))
  const typedFields = plugin.id === 'bps' ? bpsConfigFields : null

  useEffect(() => {
    setGroupIds(plugin.state.group_ids ?? [])
    setSamplePercent(sampleRateToPercent(plugin.state.capture_sample_rate))
    setConfig(plugin.state.config ?? {})
    setConfigText(JSON.stringify(plugin.state.config ?? {}, null, 2))
  }, [plugin])

  useEffect(() => {
    api.listAccountGroups().then((res) => setGroups((res.groups ?? []).filter((group) => group.channel === 'codex'))).catch(() => setGroups([]))
  }, [])

  const save = async (update: Parameters<typeof api.updateTransportPlugin>[1]) => {
    setSaving(true)
    try {
      await api.updateTransportPlugin(plugin.id, update)
      showToast(t('plugins.saved'))
      onChanged()
    } catch (err) {
      showToast(getErrorMessage(err), 'error')
    } finally {
      setSaving(false)
    }
  }

  const saveConfig = () => {
    if (typedFields) {
      void save({ config: normalizePluginConfig(config, typedFields) })
      return
    }
    const parsed = parsePluginConfigText(configText)
    if (!parsed) {
      showToast(t('plugins.configInvalid'), 'error')
      return
    }
    void save({ config: parsed })
  }

  return (
    <div className="space-y-4">
      <div className="grid grid-cols-2 gap-2.5 sm:gap-4 lg:grid-cols-4">
        <StatTile label={t('plugins.globalSwitch')} value={plugin.state.enabled ? t('plugins.globalOn') : t('plugins.globalOff')} icon={<Cable className="size-4" />} />
        <StatTile label={t('plugins.groupsCount')} value={String(plugin.state.group_ids?.length ?? 0)} />
        <StatTile label={t('plugins.overridesCount')} value={String(plugin.overrides?.length ?? 0)} />
        <StatTile label={t('plugins.captureRate')} value={plugin.state.capture_enabled ? `${sampleRateToPercent(plugin.state.capture_sample_rate)}%` : t('plugins.captureOff')} />
      </div>

      <div className="grid gap-4 lg:grid-cols-2 lg:items-start">
        <Section title={t('plugins.enablement')} description={t('plugins.enablementDesc')}>
          <div className="flex items-center justify-between gap-3 rounded-lg border border-border/60 px-3 py-2.5">
            <div>
              <div className="text-sm font-medium">{t('plugins.globalSwitch')}</div>
              <div className="text-xs text-muted-foreground">{t('plugins.globalSwitchDesc')}</div>
            </div>
            <Switch checked={plugin.state.enabled} disabled={saving} onCheckedChange={(enabled) => void save({ enabled })} aria-label={t('plugins.globalSwitch')} />
          </div>
          <Field label={t('plugins.groups')} help={t('plugins.groupsDesc')}>
            <div className="flex flex-col gap-2 sm:flex-row">
              <div className="min-w-0 flex-1">
                <AccountGroupMultiSelect
                  groups={groups}
                  value={groupIds}
                  onChange={setGroupIds}
                  placeholder={t('plugins.groupsPlaceholder')}
                  emptyLabel={t('accounts.groupsNone')}
                  selectedLabel={t('accounts.groupsSelected', { count: groupIds.length })}
                  disabled={saving}
                />
              </div>
              <Button variant="outline" size="sm" disabled={saving} onClick={() => void save({ group_ids: groupIds })}>
                <Save className="size-3.5" />
                {t('common.save')}
              </Button>
            </div>
          </Field>
        </Section>

        <Section title={t('plugins.capture')} description={t('plugins.captureDesc')}>
          <div className="flex items-center justify-between gap-3 rounded-lg border border-border/60 px-3 py-2.5">
            <div className="text-sm font-medium">{t('plugins.captureSwitch')}</div>
            <Switch checked={plugin.state.capture_enabled} disabled={saving} onCheckedChange={(capture_enabled) => void save({ capture_enabled })} aria-label={t('plugins.captureSwitch')} />
          </div>
          <Field label={t('plugins.sampleRate')} help={t('plugins.sampleRateDesc')}>
            <div className="flex gap-2">
              <DraftNumberInput className="w-28" value={samplePercent} onValueChange={setSamplePercent} min={0} max={100} disabled={saving} aria-label={t('plugins.sampleRate')} />
              <Button variant="outline" size="sm" disabled={saving} onClick={() => void save({ capture_sample_rate: sampleRateFromPercent(samplePercent) })}>
                <Save className="size-3.5" />
                {t('common.save')}
              </Button>
            </div>
          </Field>
        </Section>
      </div>

      <Section
        title={t('plugins.config')}
        description={t('plugins.configDesc')}
        actions={
          <Button size="sm" disabled={saving} onClick={saveConfig}>
            <Save className="size-3.5" />
            {t('common.save')}
          </Button>
        }
      >
        {typedFields ? (
          <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
            {typedFields.map((field) => (
              <Field key={field.key} label={t(`plugins.bpsConfig.${field.key}`)} help={field.hint ? t(`plugins.bpsConfigHints.${field.key}`) : undefined}>
                {field.kind === 'number' ? (
                  <DraftNumberInput
                    value={typeof config[field.key] === 'number' ? (config[field.key] as number) : 0}
                    onValueChange={(value) => setConfig((prev) => ({ ...prev, [field.key]: value }))}
                    min={field.min}
                    max={field.max}
                    integer
                    disabled={saving}
                    aria-label={t(`plugins.bpsConfig.${field.key}`)}
                  />
                ) : field.kind === 'boolean' ? (
                  <div className="flex h-9 items-center">
                    <Switch
                      checked={pluginConfigBoolean(config, field)}
                      onCheckedChange={(value) => setConfig((prev) => ({ ...prev, [field.key]: value }))}
                      disabled={saving}
                      aria-label={t(`plugins.bpsConfig.${field.key}`)}
                    />
                  </div>
                ) : field.kind === 'list' ? (
                  <Input
                    value={pluginConfigListText(config, field.key)}
                    onChange={(event) => setConfig((prev) => ({ ...prev, [field.key]: event.target.value }))}
                    placeholder={field.defaultValue.join(', ')}
                    disabled={saving}
                    aria-label={t(`plugins.bpsConfig.${field.key}`)}
                  />
                ) : (
                  <Input
                    value={typeof config[field.key] === 'string' ? (config[field.key] as string) : ''}
                    onChange={(event) => setConfig((prev) => ({ ...prev, [field.key]: event.target.value }))}
                    placeholder={t('plugins.defaultValue')}
                    disabled={saving}
                  />
                )}
              </Field>
            ))}
          </div>
        ) : (
          <textarea
            className="min-h-40 w-full rounded-xl border border-input bg-background p-3 font-mono text-xs focus:outline-none focus:ring-2 focus:ring-ring"
            value={configText}
            onChange={(event) => setConfigText(event.target.value)}
            aria-label={t('plugins.config')}
          />
        )}
        {typedFields && <p className="text-xs text-muted-foreground">{t('plugins.configZeroDefault')}</p>}
      </Section>

      <PluginAccounts plugin={plugin} onChanged={onChanged} />
    </div>
  )
}

const OVERRIDE_PAGE_SIZE = 20

function PluginAccounts({ plugin, onChanged }: { plugin: TransportPlugin; onChanged: () => void }) {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const [page, setPage] = useState(1)
  const [search, setSearch] = useState('')
  const [busy, setBusy] = useState<number | null>(null)
  const load = useCallback(async () => {
    const res = await api.getAccountsPage({ channel: 'codex', page, pageSize: OVERRIDE_PAGE_SIZE, search })
    return { accounts: res.accounts ?? [], total: res.total ?? 0 }
  }, [page, search])
  const { data, loading, error, reload } = useDataLoader<{ accounts: AccountRow[]; total: number }>({ initialData: { accounts: [], total: 0 }, load })
  const overrides = useMemo(() => new Map((plugin.overrides ?? []).map((item) => [item.account_id, item.enabled])), [plugin.overrides])
  const eligible = plugin.id === 'bps' ? data.accounts.filter(isBPSAccount) : data.accounts
  const [statuses, setStatuses] = useState<Map<number, PluginAccountStatus>>(new Map())
  const eligibleIds = eligible.map((account) => account.id).join(',')
  useEffect(() => {
    if (plugin.id !== 'bps' || !eligibleIds) {
      setStatuses(new Map())
      return
    }
    let active = true
    api.getPluginAccountStatus(plugin.id, eligibleIds.split(',').map(Number))
      .then((res) => { if (active) setStatuses(new Map((res.accounts ?? []).map((item) => [item.account_id, item]))) })
      .catch(() => { if (active) setStatuses(new Map()) })
    return () => { active = false }
  }, [plugin.id, eligibleIds])

  const change = async (account: AccountRow, value: BPSTriState) => {
    setBusy(account.id)
    try {
      await api.setTransportPluginAccountOverride(plugin.id, account.id, value === 'inherit' ? null : value === 'on')
      showToast(t('plugins.saved'))
      onChanged()
      await reload()
    } catch (err) {
      showToast(getErrorMessage(err), 'error')
    } finally {
      setBusy(null)
    }
  }

  return (
    <Section title={t('plugins.accounts')} description={t('plugins.accountsDesc')}>
      <div className="relative max-w-sm">
        <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" aria-hidden />
        <Input className="pl-8" value={search} onChange={(event) => { setSearch(event.target.value); setPage(1) }} placeholder={t('plugins.accountsSearch')} />
      </div>
      <StateShell loading={loading} error={error} onRetry={() => void reload()} isEmpty={!loading && eligible.length === 0} emptyTitle={t('plugins.accountsEmpty')}>
        <div className="divide-y divide-border">
          {eligible.map((account) => {
            const override = overrides.has(account.id) ? (overrides.get(account.id) ? 'on' : 'off') : 'inherit'
            return (
              <div key={account.id} className="flex flex-col gap-2 py-2.5 sm:flex-row sm:items-center sm:justify-between">
                <div className="min-w-0 text-sm">
                  <span className="block truncate">{account.name || account.email || `#${account.id}`}</span>
                  <span className="text-xs text-muted-foreground">
                    #{account.id}
                    {account.plan_type ? ` · ${account.plan_type}` : ''}
                    {plugin.id === 'bps' && ` · ${account.codex_bps_active ? t('accounts.bps.activeNow') : t('accounts.bps.inactiveNow')}`}
                  </span>
                  <PluginAccountStatusLine status={statuses.get(account.id)} />
                </div>
                <Select
                  className="w-full sm:w-44"
                  compact
                  value={override}
                  onValueChange={(value) => void change(account, value as BPSTriState)}
                  options={[
                    { value: 'inherit', label: t('plugins.inherit') },
                    { value: 'on', label: t('plugins.forceOn') },
                    { value: 'off', label: t('plugins.forceOff') },
                  ]}
                  disabled={busy !== null}
                  aria-label={t('plugins.overrideFor', { id: account.id })}
                />
              </div>
            )
          })}
        </div>
      </StateShell>
      <Pagination page={page} totalPages={Math.ceil(data.total / OVERRIDE_PAGE_SIZE)} onPageChange={setPage} totalItems={data.total} pageSize={OVERRIDE_PAGE_SIZE} />
    </Section>
  )
}

// PluginAccountStatusLine shows an account's plugin-scoped cooldown.
function PluginAccountStatusLine({ status }: { status?: PluginAccountStatus }) {
  const { t } = useTranslation()
  const models = Object.entries(status?.models_unavailable ?? {})
  if (!status || (!status.cooling_until && !status.policy_strikes && !status.policy_tier && models.length === 0)) return null
  return (
    <span className="mt-1 block space-y-0.5 text-xs text-amber-600 dark:text-amber-400">
      {Boolean(status.cooling_until || status.policy_strikes) && (
        <span className="block">
          {status.cooling_until
            ? t('plugins.coolingUntil', { time: formatBeijingTime(status.cooling_until), reason: t(pluginCoolingReasonKey(status.reason), { defaultValue: status.reason ?? '' }) })
            : t('plugins.policyStrikes', { count: status.policy_strikes })}
        </span>
      )}
      {Boolean(status.policy_tier) && <span className="block">{t('plugins.policyTier', { tier: status.policy_tier, tiers: status.policy_tiers })}</span>}
      {models.map(([model, until]) => (
        <span key={model} className="block break-all">{t('plugins.modelUnavailable', { model, time: formatBeijingTime(until) })}</span>
      ))}
    </span>
  )
}

function TimeRangeTabs({ value, onChange }: { value: TimeRangeKey; onChange: (value: TimeRangeKey) => void }) {
  const { t } = useTranslation()
  return (
    <SegmentedTabs
      size="sm"
      tabs={TIME_RANGES.map((key) => ({ value: key, label: t(`dashboard.timeRange${key.toUpperCase()}`) }))}
      value={value}
      onValueChange={(next) => onChange(next as TimeRangeKey)}
    />
  )
}

const CAPTURE_PAGE_SIZE = 50

// CaptureCleanup shows how much the plugin's captures take and purges them
// on demand (older than N hours, errors only, or all) after confirmation.
function CaptureCleanup({ plugin, onPurged }: { plugin: TransportPlugin; onPurged: () => void }) {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const { confirm, confirmDialog } = useConfirmDialog()
  const [mode, setMode] = useState<PluginCapturePurgeMode>('older_than')
  const [hours, setHours] = useState(6)
  const [busy, setBusy] = useState(false)
  const loadStats = useCallback(() => api.getPluginCaptureStats(plugin.id), [plugin.id])
  const { data: stats, reload: reloadStats } = useDataLoader<PluginCaptureStats | null>({ initialData: null, load: loadStats })

  const purge = async () => {
    const description = mode === 'older_than' ? t('plugins.purgeConfirmOlder', { hours }) : t(`plugins.purgeConfirm_${mode}`)
    if (!await confirm({ title: t('plugins.purgeTitle'), description, confirmText: t('plugins.purge'), tone: 'destructive', confirmVariant: 'destructive' })) return
    setBusy(true)
    try {
      const result = await api.purgePluginCaptures(plugin.id, mode, mode === 'older_than' ? hours : undefined)
      showToast(t('plugins.purged', { count: result.deleted }))
      onPurged()
      await reloadStats()
    } catch (err) {
      showToast(getErrorMessage(err), 'error')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Card>
      <CardContent className="flex flex-col gap-3 p-4 lg:flex-row lg:items-center lg:justify-between">
        <p className="text-sm text-muted-foreground">
          {stats
            ? t('plugins.captureStats', {
              rows: stats.rows,
              errors: stats.error_rows,
              size: formatCaptureBytes(stats.table_bytes > 0 ? stats.table_bytes : stats.body_bytes),
            })
            : t('plugins.captureStatsLoading')}
        </p>
        <div className="flex flex-wrap items-center gap-2">
          <Select
            className="w-full sm:w-44"
            compact
            value={mode}
            onValueChange={(value) => setMode(value as PluginCapturePurgeMode)}
            options={CAPTURE_PURGE_MODES.map((value) => ({ value, label: t(`plugins.purgeModes.${value}`) }))}
            disabled={busy}
            aria-label={t('plugins.purgeMode')}
          />
          {mode === 'older_than' && (
            <DraftNumberInput
              className="w-24"
              value={hours}
              onValueChange={setHours}
              min={1}
              max={720}
              integer
              disabled={busy}
              aria-label={t('plugins.purgeHours')}
            />
          )}
          <Button variant="destructive" size="sm" onClick={() => void purge()} disabled={busy}>
            <Trash2 className="size-3.5" />
            {t('plugins.purge')}
          </Button>
        </div>
      </CardContent>
      {confirmDialog}
    </Card>
  )
}

function PluginCaptures({ plugin }: { plugin: TransportPlugin }) {
  const { t } = useTranslation()
  const [timeRange, setTimeRange] = useState<TimeRangeKey>('24h')
  const [requestId, setRequestId] = useState('')
  const [accountId, setAccountId] = useState('')
  const [status, setStatus] = useState('')
  const [direction, setDirection] = useState('')
  const [page, setPage] = useState(1)
  const [detail, setDetail] = useState<PluginCapture | null>(null)
  const load = useCallback(async () => {
    const range = getTimeRangeISO(timeRange)
    return api.getPluginCaptures(plugin.id, {
      requestId: requestId.trim() || undefined,
      accountId: Number(accountId) > 0 ? Number(accountId) : undefined,
      status: status.trim() !== '' && Number.isInteger(Number(status)) ? Number(status) : undefined,
      direction: (direction || undefined) as PluginCapture['direction'] | undefined,
      start: range.start,
      end: range.end,
      page,
      pageSize: CAPTURE_PAGE_SIZE,
    })
  }, [accountId, direction, page, plugin.id, requestId, status, timeRange])
  const { data, loading, error, reload } = useDataLoader({ initialData: { captures: [] as PluginCapture[], total: 0 }, load })

  const openDetail = async (capture: PluginCapture) => {
    setDetail(capture)
    try {
      setDetail(await api.getPluginCapture(plugin.id, capture.id))
    } catch {
      // Keep the list row; the body is simply unavailable.
    }
  }
  const openEvidence = (evidenceId: string) => {
    const id = captureIdFromEvidence(evidenceId)
    if (id) void api.getPluginCapture(plugin.id, id).then(setDetail).catch(() => undefined)
  }
  const agentFilters = pluginCaptureAgentFilters({ requestId, accountId, status, direction })

  return (
    <div className="space-y-4">
      <CaptureCleanup plugin={plugin} onPurged={() => { setPage(1); void reload() }} />
      {!plugin.state.capture_enabled && (
        <p role="status" className="rounded-lg border border-border/60 bg-muted/40 px-3 py-2 text-xs text-muted-foreground">
          {t('plugins.captureDisabledHint')}
        </p>
      )}
      <Card>
        <CardContent className="space-y-3 p-4">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <TimeRangeTabs value={timeRange} onChange={(value) => { setTimeRange(value); setPage(1) }} />
            <Button variant="outline" size="sm" onClick={() => void reload()}>
              <RefreshCw className={loading ? 'size-3.5 animate-spin' : 'size-3.5'} />
              {t('common.refresh')}
            </Button>
          </div>
          <div className="grid gap-2 sm:grid-cols-2 lg:grid-cols-4">
            <Input value={requestId} onChange={(event) => { setRequestId(event.target.value); setPage(1) }} placeholder={t('plugins.filterRequestId')} aria-label={t('plugins.filterRequestId')} />
            <Input value={accountId} onChange={(event) => { setAccountId(event.target.value); setPage(1) }} placeholder={t('plugins.filterAccountId')} inputMode="numeric" aria-label={t('plugins.filterAccountId')} />
            <Input value={status} onChange={(event) => { setStatus(event.target.value); setPage(1) }} placeholder={t('plugins.filterStatus')} inputMode="numeric" aria-label={t('plugins.filterStatus')} />
            <Select
              value={direction}
              onValueChange={(value) => { setDirection(value); setPage(1) }}
              options={[
                { value: '', label: t('plugins.directionAll') },
                { value: 'request', label: t('plugins.directions.request') },
                { value: 'upstream_request', label: t('plugins.directions.upstream_request') },
                { value: 'response', label: t('plugins.directions.response') },
                { value: 'error', label: t('plugins.directions.error') },
              ]}
              aria-label={t('plugins.filterDirection')}
            />
          </div>
        </CardContent>
      </Card>
      <LogAgentPanel
        source={pluginCaptureSource(plugin.id)}
        filters={agentFilters}
        getRange={() => getTimeRangeISO(timeRange)}
        description={t('plugins.agentCapturesDesc')}
        onEvidenceClick={openEvidence}
      />
      <StateShell loading={loading} error={error} onRetry={() => void reload()} isEmpty={!loading && data.captures.length === 0} emptyTitle={t('plugins.capturesEmpty')}>
        <Card>
          <CardContent className="p-0">
            <div className="overflow-x-auto">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>{t('plugins.colTime')}</TableHead>
                    <TableHead>{t('plugins.colDirection')}</TableHead>
                    <TableHead>{t('plugins.colStatus')}</TableHead>
                    <TableHead>{t('plugins.colAccount')}</TableHead>
                    <TableHead>{t('plugins.colRequest')}</TableHead>
                    <TableHead className="text-right">{t('plugins.colBytes')}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {data.captures.map((capture) => (
                    <TableRow key={capture.id} className="cursor-pointer" onClick={() => void openDetail(capture)}>
                      <TableCell className="whitespace-nowrap text-xs">{formatBeijingTime(capture.created_at)}</TableCell>
                      <TableCell>
                        <Badge variant={capture.direction === 'error' ? 'destructive' : 'secondary'}>{t(`plugins.directions.${capture.direction}`)}</Badge>
                      </TableCell>
                      <TableCell className="text-xs tabular-nums">{capture.status || '—'}{capture.error_kind ? ` · ${capture.error_kind}` : ''}</TableCell>
                      <TableCell className="text-xs tabular-nums">#{capture.account_id} · {t('plugins.attempt', { n: capture.attempt })}</TableCell>
                      <TableCell className={monoClass}><span className="block max-w-[220px] truncate">{capture.request_id || '—'}</span></TableCell>
                      <TableCell className="text-right text-xs tabular-nums">
                        {capture.body_bytes}
                        {capture.truncated && <span className="ml-1 text-muted-foreground">{t('plugins.truncated')}</span>}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
          </CardContent>
        </Card>
      </StateShell>
      <Pagination page={page} totalPages={Math.ceil(data.total / CAPTURE_PAGE_SIZE)} onPageChange={setPage} totalItems={data.total} pageSize={CAPTURE_PAGE_SIZE} />
      <Dialog open={detail !== null} onOpenChange={(open) => { if (!open) setDetail(null) }}>
        <DialogContent className="max-h-[85vh] overflow-y-auto sm:max-w-[760px]">
          <DialogHeader>
            <DialogTitle>{t('plugins.captureDetail')}</DialogTitle>
            <DialogDescription className={monoClass}>{detail?.request_id || '—'}</DialogDescription>
          </DialogHeader>
          {detail && (
            <div className="space-y-3 text-xs">
              <div className="flex flex-wrap gap-2">
                <Badge variant="secondary">{t(`plugins.directions.${detail.direction}`)}</Badge>
                {detail.status > 0 && <Badge variant="secondary">HTTP {detail.status}</Badge>}
                {detail.truncated && <Badge variant="secondary">{t('plugins.truncated')}</Badge>}
                <span className="text-muted-foreground">{formatBeijingTime(detail.created_at)}</span>
              </div>
              <div>
                <div className="mb-1 font-medium">{t('plugins.headers')}</div>
                <pre className="max-h-60 overflow-auto whitespace-pre-wrap break-all rounded-lg bg-muted/50 p-3 font-mono">{prettyJSON(detail.headers)}</pre>
              </div>
              <div>
                <div className="mb-1 font-medium">{t('plugins.body')}</div>
                <pre className="max-h-96 overflow-auto whitespace-pre-wrap break-all rounded-lg bg-muted/50 p-3 font-mono">{detail.body ?? ''}</pre>
              </div>
              {detail.request_id && (
                <LogAgentPanel
                  key={detail.request_id}
                  bare
                  showHistory={false}
                  source={pluginCaptureSource(plugin.id)}
                  refs={[detail.request_id]}
                  title={t('plugins.agentRequestTitle')}
                  description={t('plugins.agentRequestDesc')}
                />
              )}
            </div>
          )}
        </DialogContent>
      </Dialog>
    </div>
  )
}

function prettyJSON(text: string) {
  try {
    return JSON.stringify(JSON.parse(text), null, 2)
  } catch {
    return text
  }
}

const LOG_PAGE_SIZE = 20

function PluginLogs({ plugin }: { plugin: TransportPlugin }) {
  const { t } = useTranslation()
  const [timeRange, setTimeRange] = useState<TimeRangeKey>('24h')
  const [page, setPage] = useState(1)
  const load = useCallback(async () => {
    const range = getTimeRangeISO(timeRange)
    const res = await api.getUsageLogsPaged({ ...range, transport: plugin.id, page, pageSize: LOG_PAGE_SIZE })
    return { logs: res.logs ?? [], total: res.total ?? 0 }
  }, [page, plugin.id, timeRange])
  const { data, loading, error, reload } = useDataLoader<{ logs: UsageLog[]; total: number }>({ initialData: { logs: [], total: 0 }, load })
  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <TimeRangeTabs value={timeRange} onChange={(value) => { setTimeRange(value); setPage(1) }} />
        <Button variant="outline" size="sm" onClick={() => void reload()}>
          <RefreshCw className={loading ? 'size-3.5 animate-spin' : 'size-3.5'} />
          {t('common.refresh')}
        </Button>
      </div>
      <StateShell loading={loading} error={error} onRetry={() => void reload()} isEmpty={!loading && data.logs.length === 0} emptyTitle={t('plugins.logsEmpty')}>
        <Card>
          <CardContent className="p-0">
            <div className="overflow-x-auto">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>{t('plugins.colTime')}</TableHead>
                    <TableHead>{t('plugins.colStatus')}</TableHead>
                    <TableHead>{t('plugins.colAccount')}</TableHead>
                    <TableHead>{t('plugins.colModel')}</TableHead>
                    <TableHead className="text-right">{t('plugins.colTokens')}</TableHead>
                    <TableHead className="text-right">{t('plugins.colDuration')}</TableHead>
                    <TableHead>{t('plugins.colMeta')}</TableHead>
                    <TableHead>{t('plugins.colRequest')}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {data.logs.map((log) => (
                    <TableRow key={log.id}>
                      <TableCell className="whitespace-nowrap text-xs">{formatBeijingTime(log.created_at)}</TableCell>
                      <TableCell className="text-xs tabular-nums">
                        <Badge variant={log.status_code >= 400 ? 'destructive' : 'secondary'}>{log.status_code}</Badge>
                        {log.upstream_error_kind && <span className="ml-1 text-muted-foreground">{log.upstream_error_kind}</span>}
                      </TableCell>
                      <TableCell className="text-xs"><span className="block max-w-[180px] truncate">{log.account_name || log.account_email || `#${log.account_id}`}</span></TableCell>
                      <TableCell className="text-xs">{log.model}</TableCell>
                      <TableCell className="text-right text-xs tabular-nums">{log.input_tokens} / {log.output_tokens}</TableCell>
                      <TableCell className="text-right text-xs tabular-nums">{log.duration_ms} ms</TableCell>
                      <TableCell className={monoClass}>{pluginMetaSummary(log.plugin_meta) || '—'}</TableCell>
                      <TableCell className={monoClass}><span className="block max-w-[200px] truncate">{log.request_id || '—'}</span></TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
          </CardContent>
        </Card>
      </StateShell>
      <Pagination page={page} totalPages={Math.ceil(data.total / LOG_PAGE_SIZE)} onPageChange={setPage} totalItems={data.total} pageSize={LOG_PAGE_SIZE} />
    </div>
  )
}

function PluginAgent({ plugin }: { plugin: TransportPlugin }) {
  const { t } = useTranslation()
  const [timeRange, setTimeRange] = useState<TimeRangeKey>('24h')
  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <p className="text-xs text-muted-foreground">{t('plugins.agentDesc', { name: plugin.meta.name })}</p>
        <TimeRangeTabs value={timeRange} onChange={setTimeRange} />
      </div>
      <LogAgentPanel
        source="ops_errors"
        filters={{ transport: plugin.id }}
        getRange={() => getTimeRangeISO(timeRange)}
        title={t('plugins.agentErrorsTitle')}
        description={t('plugins.agentErrorsDesc')}
      />
      <LogAgentPanel
        source={pluginCaptureSource(plugin.id)}
        getRange={() => getTimeRangeISO(timeRange)}
        title={t('plugins.agentCapturesTitle')}
        description={t('plugins.agentCapturesDesc')}
      />
    </div>
  )
}
