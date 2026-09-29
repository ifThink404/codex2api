import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react'
import { Link, Navigate, useParams } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import { Archive, Cable, ChevronRight, Fingerprint, Gauge, HeartPulse, Hourglass, Paperclip, RefreshCw, Route, Save, Search, ShieldAlert, ShieldCheck, Timer, Trash2 } from 'lucide-react'
import { Area, CartesianGrid, ComposedChart, Legend, Line, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts'
import { api } from '../api'
import AccountGroupMultiSelect from '../components/AccountGroupMultiSelect'
import LogAgentPanel from '../components/LogAgentPanel'
import PageHeader from '../components/PageHeader'
import Pagination from '../components/Pagination'
import StateShell from '../components/StateShell'
import { SegmentedTabs } from '../components/SegmentedTabs'
import { StatTile } from '../components/StatTile'
import StatCard from '../components/StatCard'
import SystemHealthBar from '../components/SystemHealthBar'
import { Chip as RunwayChip } from '../components/PoolRunwayCard'
import { DEFAULT_PAGE_SIZE_OPTIONS, usePersistedPageSize } from '../hooks/usePersistedPageSize'
import { axisColor, chartMargin, gridColor, tooltipContentStyle, tooltipItemStyle, tooltipLabelStyle } from '../lib/chartTheme'
import { riskPalette } from '../lib/riskPalette'
import type { RiskLevel } from '../lib/poolRunway'
import { useDataLoader } from '../hooks/useDataLoader'
import { useConfirmDialog } from '../hooks/useConfirmDialog'
import { useToast } from '../hooks/useToast'
import { isBPSAccount, type BPSTriState } from '../lib/bpsAccount'
import {
  PLUGIN_VIEWS,
  bpsConfigFields,
  bpsConfigGroups,
  type PluginConfigField,
  CAPTURE_PURGE_MODES,
  formatBlockDuration,
  BPS_STATE_BADGE_CLASSES,
  BPS_TRAFFIC_RANGES,
  type BPSTrafficRange,
  activeUntil,
  bpsHealthTimeline,
  bpsTrafficSeries,
  capacityFill,
  hasTime,
  formatWindowLabel,
  activityBarPercent,
  formatCaptureBytes,
  secondsSince,
  formatSuccessRate,
  liveElapsedSeconds,
  secondsUntil,
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
import { SETTINGS_FIELD_GRID, SETTINGS_ROW_LIST, SettingField, SettingsCard } from '../components/SettingsLayout'
import type { AccountGroup, AccountRow, BPSActivity, BPSActivityAccount, BPSDashboard, BPSPolicyBlocksResponse, PluginAccountStatus, PluginCapture, PluginCapturePurgeMode, PluginCaptureStats, TransportPlugin, UsageLog } from '../types'
import { Badge } from '@/components/ui/badge'
import { cn } from '@/lib/utils'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { DraftNumberInput } from '@/components/ui/draft-number-input'
import { Input } from '@/components/ui/input'
import { SegmentedPillGroup } from '@/components/ui/segmented-pill-group'
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

// Icons of the BPS config groups (bpsConfigGroups).
const BPS_CONFIG_GROUP_ICONS: Record<string, ReactNode> = {
  routing: <Route />,
  protection: <ShieldCheck />,
  attachments: <Paperclip />,
  identity: <Fingerprint />,
  captures: <Archive />,
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

  // configFieldHint is the field's own hint plus, for numbers, what 0 means.
  const configFieldHint = (field: PluginConfigField): string => {
    const parts: string[] = []
    if (field.hint) parts.push(t(`plugins.bpsConfigHints.${field.key}`))
    if (field.kind === 'number' && field.zeroMeans === 'default' && field.defaultValue !== undefined) parts.push(t('plugins.zeroMeansDefault', { value: field.defaultValue }))
    if (field.kind === 'number' && field.zeroMeans === 'off') parts.push(t('plugins.zeroMeansOff'))
    return parts.join(' ')
  }
  const renderConfigInput = (field: PluginConfigField) => {
    switch (field.kind) {
      case 'number':
        return (
          <DraftNumberInput
            value={typeof config[field.key] === 'number' ? (config[field.key] as number) : 0}
            onValueChange={(value) => setConfig((prev) => ({ ...prev, [field.key]: value }))}
            min={field.min}
            max={field.max}
            integer
            disabled={saving}
            aria-label={t(`plugins.bpsConfig.${field.key}`)}
          />
        )
      case 'list':
        return (
          <Input
            value={pluginConfigListText(config, field.key)}
            onChange={(event) => setConfig((prev) => ({ ...prev, [field.key]: event.target.value }))}
            placeholder={field.defaultValue.join(', ')}
            disabled={saving}
            aria-label={t(`plugins.bpsConfig.${field.key}`)}
          />
        )
      default:
        return (
          <Input
            value={typeof config[field.key] === 'string' ? (config[field.key] as string) : ''}
            onChange={(event) => setConfig((prev) => ({ ...prev, [field.key]: event.target.value }))}
            placeholder={field.kind === 'text' && field.placeholder ? field.placeholder : t('plugins.defaultValue')}
            disabled={saving}
            aria-label={t(`plugins.bpsConfig.${field.key}`)}
          />
        )
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
      {plugin.id === 'bps' && <BPSDashboardPanel plugin={plugin} />}
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
          <div className="space-y-4">
            {bpsConfigGroups.map((group) => {
              const fields = group.fields.map((key) => typedFields.find((field) => field.key === key)).filter((field): field is PluginConfigField => Boolean(field))
              const inputs = fields.filter((field) => field.kind !== 'boolean')
              const toggles = fields.filter((field) => field.kind === 'boolean')
              return (
                <SettingsCard
                  key={group.key}
                  title={t(`plugins.bpsConfigGroups.${group.key}.title`)}
                  description={t(`plugins.bpsConfigGroups.${group.key}.description`)}
                  icon={BPS_CONFIG_GROUP_ICONS[group.key]}
                >
                  {inputs.length > 0 && (
                    <div className={SETTINGS_FIELD_GRID}>
                      {inputs.map((field) => (
                        <div key={field.key} className="flex min-w-0 flex-col gap-1.5">
                          <SettingField label={t(`plugins.bpsConfig.${field.key}`)}>{renderConfigInput(field)}</SettingField>
                          {configFieldHint(field) && <p className="text-xs leading-relaxed text-muted-foreground">{configFieldHint(field)}</p>}
                        </div>
                      ))}
                    </div>
                  )}
                  {toggles.length > 0 && (
                    <div className={cn(SETTINGS_ROW_LIST, inputs.length > 0 && 'mt-5 border-t border-border/60 pt-4')}>
                      {toggles.map((field) => (
                        <SettingField key={field.key} layout="row" label={t(`plugins.bpsConfig.${field.key}`)} description={configFieldHint(field)}>
                          <Switch
                            checked={pluginConfigBoolean(config, field)}
                            onCheckedChange={(value) => setConfig((prev) => ({ ...prev, [field.key]: value }))}
                            disabled={saving}
                            aria-label={t(`plugins.bpsConfig.${field.key}`)}
                          />
                        </SettingField>
                      ))}
                    </div>
                  )}
                </SettingsCard>
              )
            })}
          </div>
        ) : (
          <textarea
            className="min-h-40 w-full rounded-xl border border-input bg-background p-3 font-mono text-xs focus:outline-none focus:ring-2 focus:ring-ring"
            value={configText}
            onChange={(event) => setConfigText(event.target.value)}
            aria-label={t('plugins.config')}
          />
        )}
      </Section>

      <PluginAccounts plugin={plugin} onChanged={onChanged} />
      {plugin.id === 'bps' && <PolicyBlocks plugin={plugin} />}
    </div>
  )
}

const POLICY_BLOCKS_REFRESH_MS = 30_000

const BPS_DASHBOARD_REFRESH_MS = 15_000

// bpsHealthRisk grades usable BPS accounts for the hero: none usable is
// high risk, at or under the warning floor medium, otherwise low.
function bpsHealthRisk(summary: BPSDashboard['summary'] | undefined): RiskLevel {
  if (!summary || summary.total === 0) return 'low'
  if (summary.usable === 0) return 'high'
  return summary.warning ? 'medium' : 'low'
}

// BPSDashboard is the BPS health dashboard: a usable-accounts hero against
// the warning floor, a KPI row, traffic charts (1h / 24h) and the live
// activity table. Elapsed times tick every second; data refreshes every 15s.
function BPSDashboardPanel({ plugin }: { plugin: TransportPlugin }) {
  const { t } = useTranslation()
  const [data, setData] = useState<BPSDashboard | null>(null)
  const [fetchedAt, setFetchedAt] = useState(() => Date.now())
  const [now, setNow] = useState(() => Date.now())
  const [error, setError] = useState('')
  const [range, setRange] = useState<BPSTrafficRange>('1h')
  useEffect(() => {
    let active = true
    const load = () => api.getBPSDashboard(plugin.id)
      .then((res) => { if (active) { setData(res); setFetchedAt(Date.now()); setError('') } })
      .catch((err) => { if (active) setError(getErrorMessage(err)) })
    void load()
    const refresh = window.setInterval(() => void load(), BPS_DASHBOARD_REFRESH_MS)
    const tick = window.setInterval(() => setNow(Date.now()), 1000)
    return () => { active = false; window.clearInterval(refresh); window.clearInterval(tick) }
  }, [plugin.id])
  const summary = data?.summary
  const risk = bpsHealthRisk(summary)
  const palette = riskPalette(risk)
  const traffic = data?.traffic[range]
  const points = useMemo(() => data?.timeline?.[range] ?? [], [data, range])
  const series = useMemo(() => bpsTrafficSeries(points, range, fetchedAt), [points, range, fetchedAt])
  const healthData = useMemo(() => ({ timeline: bpsHealthTimeline(points), models: [] }), [points])
  const inFlight = (data?.accounts ?? []).reduce((sum, account) => sum + account.in_flight, 0)
  const recovery = data?.recovery
  const heroState = !summary
    ? t('common.loading')
    : summary.total === 0
      ? t('plugins.dashNoAccounts')
      : t(summary.warning ? 'plugins.dashUsableWarning' : 'plugins.dashHealthy', { floor: summary.min_usable })

  return (
    <Section title={t('plugins.dashTitle')} description={t('plugins.dashDesc')}>
      {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
      <div className="relative overflow-hidden rounded-2xl border border-border/80 bg-card p-4 shadow-sm sm:p-5">
        <div aria-hidden className={cn('pointer-events-none absolute inset-0 opacity-90', palette.wash)} />
        <div className="relative z-10 flex flex-col gap-4 lg:flex-row lg:items-center lg:justify-between">
          <div className="min-w-0">
            <div className="text-[11px] font-bold uppercase tracking-wide text-muted-foreground">{t('plugins.dashUsable')}</div>
            <div className="mt-2 flex flex-wrap items-end gap-x-3 gap-y-1">
              <div className={cn('text-3xl font-bold tabular-nums tracking-tight sm:text-4xl', palette.fg)}>{summary ? summary.usable : '—'}</div>
              <div className="pb-1 text-sm font-medium text-muted-foreground">{t('plugins.dashUsableOf', { total: summary?.total ?? 0 })}</div>
            </div>
            <div className="mt-2.5 flex flex-wrap items-center gap-2">
              <span className={cn('inline-flex items-center gap-1.5 rounded-full px-2.5 py-1 text-xs font-semibold', palette.pill)}>
                <span className={cn('size-1.5 rounded-full', palette.dot)} />
                {heroState}
              </span>
            </div>
          </div>
          <div className="flex flex-wrap gap-2 lg:max-w-2xl lg:justify-end">
            <RunwayChip label={t('plugins.dashFloor')} value={String(summary?.min_usable ?? '—')} />
            {Boolean(summary?.disabled) && <RunwayChip label={t('plugins.dashDisabled')} value={String(summary?.disabled)} />}
            <RunwayChip label={t('plugins.dashInFlightTotal')} value={String(inFlight)} emphasize={inFlight > 0} />
            <RunwayChip label={t('plugins.dashRequests1h')} value={String(data?.traffic['1h'].requests ?? 0)} />
            <RunwayChip label={t('plugins.dashSuccessRate1h')} value={data ? formatSuccessRate(data.traffic['1h'].success_rate, data.traffic['1h'].requests) : '—'} />
          </div>
        </div>
      </div>

      <div className="grid grid-cols-2 gap-2.5 sm:gap-4 md:grid-cols-3 xl:grid-cols-5">
        <StatCard icon={<ShieldAlert />} iconClass={summary?.policy_blocked ? 'red' : 'green'} label={t('plugins.dashPolicyBlocked')} value={summary?.policy_blocked ?? 0} />
        <StatCard icon={<Timer />} iconClass={summary?.rate_cooling ? 'amber' : 'green'} label={t('plugins.dashRateCooling')} value={summary?.rate_cooling ?? 0} />
        <StatCard icon={<Gauge />} iconClass={summary?.budget_exhausted ? 'amber' : 'green'} label={t('plugins.dashBudgetExhausted')} value={summary?.budget_exhausted ?? 0} />
        <StatCard
          icon={<Hourglass />}
          iconClass={recovery?.blocked ? 'red' : 'green'}
          label={t('plugins.dashLongestActive')}
          value={recovery?.blocked ? formatBlockDuration(liveElapsedSeconds(recovery.longest_active_seconds, fetchedAt, now)) : '—'}
          sub={t('plugins.dashBlockedNow', { count: recovery?.blocked ?? 0 })}
        />
        <StatCard
          icon={<HeartPulse />}
          iconClass="purple"
          label={t('plugins.dashRecovered')}
          value={recovery?.recovered ?? 0}
          sub={recovery?.recovered
            ? t('plugins.dashRecoveryRange', {
              min: formatBlockDuration(recovery.min_seconds),
              median: formatBlockDuration(recovery.median_seconds),
              max: formatBlockDuration(recovery.max_seconds),
            })
            : t('plugins.dashNoRecovery')}
          className="col-span-2 min-[420px]:col-span-1 md:col-span-1"
        />
      </div>

      <Card className="py-0 border-border/70 bg-card shadow-2xs">
        <CardContent className="space-y-4 p-3.5 sm:p-5">
          <div className="flex flex-wrap items-start justify-between gap-3">
            <div className="min-w-0">
              <h4 className="text-sm font-bold tracking-tight text-foreground">{t('plugins.dashTraffic')}</h4>
              <p className="mt-0.5 text-xs leading-relaxed text-muted-foreground/90">{t('plugins.dashTrafficDesc')}</p>
            </div>
            <SegmentedPillGroup
              label={t('plugins.dashTrafficRange')}
              value={range}
              onChange={setRange}
              options={BPS_TRAFFIC_RANGES.map((key) => ({ value: key, label: key }))}
              className="w-28 shrink-0"
            />
          </div>
          <div className="flex flex-wrap gap-2">
            <RunwayChip label={t('plugins.dashRequests')} value={String(traffic?.requests ?? 0)} />
            <RunwayChip label={t('plugins.dashSuccessRate')} value={traffic ? formatSuccessRate(traffic.success_rate, traffic.requests) : '—'} />
            <RunwayChip label={t('plugins.dashOrg429')} value={String(traffic?.org_rate_limited ?? 0)} emphasize={Boolean(traffic?.org_rate_limited)} />
            <RunwayChip label={t('plugins.dashAccount429')} value={String(traffic?.rate_limited ?? 0)} emphasize={Boolean(traffic?.rate_limited)} />
            <RunwayChip label={t('plugins.dashPolicyBlocks')} value={String(traffic?.policy_blocked ?? 0)} emphasize={Boolean(traffic?.policy_blocked)} />
            <RunwayChip label={t('plugins.dashFirstToken')} value={traffic?.avg_first_token_ms ? `${traffic.avg_first_token_ms} ms` : '—'} />
          </div>
          <div className="h-[220px] sm:h-[260px]">
            <ResponsiveContainer width="100%" height="100%">
              <ComposedChart data={series} margin={chartMargin}>
                <defs>
                  <linearGradient id="bps-request-gradient" x1="0" y1="0" x2="0" y2="1">
                    <stop offset="5%" stopColor="var(--color-primary)" stopOpacity={0.28} />
                    <stop offset="95%" stopColor="var(--color-primary)" stopOpacity={0} />
                  </linearGradient>
                </defs>
                <CartesianGrid vertical={false} stroke={gridColor} strokeDasharray="4 4" />
                <XAxis dataKey="label" tick={{ fill: axisColor, fontSize: 12 }} axisLine={{ stroke: gridColor }} tickLine={{ stroke: gridColor }} minTickGap={20} tickMargin={8} />
                <YAxis tick={{ fill: axisColor, fontSize: 12 }} axisLine={{ stroke: gridColor }} tickLine={{ stroke: gridColor }} allowDecimals={false} width={40} />
                <Tooltip
                  position={{ y: 10 }}
                  labelFormatter={(_, payload) => String(payload?.[0]?.payload?.fullLabel ?? '')}
                  contentStyle={tooltipContentStyle}
                  labelStyle={tooltipLabelStyle}
                  itemStyle={tooltipItemStyle}
                />
                <Legend wrapperStyle={{ paddingTop: 12, fontSize: 12 }} />
                <Area type="monotone" dataKey="requests" name={t('plugins.dashSeriesRequests')} stroke="var(--color-primary)" fill="url(#bps-request-gradient)" strokeWidth={2.5} />
                <Line type="monotone" dataKey="succeeded" name={t('plugins.dashSeriesSucceeded')} stroke="hsl(var(--success))" strokeWidth={2} dot={false} activeDot={{ r: 4 }} />
                <Line type="monotone" dataKey="errors4xx" name={t('plugins.dashSeries4xx')} stroke="var(--color-destructive)" strokeWidth={2} dot={false} activeDot={{ r: 4 }} />
                <Line type="monotone" dataKey="errors5xx" name={t('plugins.dashSeries5xx')} stroke="hsl(var(--warning))" strokeWidth={2} dot={false} activeDot={{ r: 4 }} />
                <Line type="monotone" dataKey="org429" name={t('plugins.dashOrg429')} stroke="hsl(var(--info))" strokeWidth={2} strokeDasharray="4 3" dot={false} activeDot={{ r: 4 }} />
                <Line type="monotone" dataKey="policyBlocked" name={t('plugins.dashPolicyBlocks')} stroke="var(--color-destructive)" strokeWidth={2} strokeDasharray="2 3" dot={false} activeDot={{ r: 4 }} />
              </ComposedChart>
            </ResponsiveContainer>
          </div>
        </CardContent>
      </Card>
      <SystemHealthBar chartData={healthData} timeRange={range} loading={!data} title={t('plugins.dashSuccessStrip')} />

      <BPSActivityPanel plugin={plugin} />
    </Section>
  )
}

const BPS_ACTIVITY_REFRESH_MS = 2_500

// InFlightBar is the capacity bar (APIKeyModelRequestUsage) for requests in
// flight: the live gradient while busy, destructive at the cap. Without a
// cap it scales to the busiest account.
function InFlightBar({ value, limit, busiest, label }: { value: number; limit: number; busiest: number; label: string }) {
  const percent = activityBarPercent(value, limit, busiest)
  const fill = capacityFill(value, limit)
  return (
    <div className="min-w-[8rem] space-y-1">
      <div className="h-1.5 overflow-hidden rounded-full bg-muted" role="meter" aria-label={label} aria-valuenow={value} aria-valuemin={0} aria-valuemax={limit || undefined}>
        <div
          className={cn('h-full rounded-full transition-[width] duration-500', fill === 'over'
            ? 'bg-destructive'
            : fill === 'live' ? 'bg-gradient-to-r from-sky-400 via-violet-400 to-sky-400 bg-[length:200%_100%] animate-pulse' : 'bg-primary')}
          style={{ width: `${percent}%` }}
        />
      </div>
      <div className={cn('text-xs tabular-nums', fill === 'over' ? 'font-medium text-destructive' : 'text-muted-foreground')}>
        {limit ? `${value} / ${limit}` : String(value)}
      </div>
    </div>
  )
}

// BudgetBar is the APIKeyModelRequestUsage bar for successful requests
// against the budget, with the used / limit caption and remaining label.
function BudgetBar({ used, limit, busiest, label }: { used: number; limit: number; busiest: number; label: string }) {
  const { t } = useTranslation()
  const remaining = limit - used
  const over = limit > 0 && remaining <= 0
  return (
    <div className="min-w-[10rem] space-y-1">
      <div className="h-1.5 overflow-hidden rounded-full bg-muted" role="meter" aria-label={label} aria-valuenow={used} aria-valuemin={0} aria-valuemax={limit || undefined}>
        <div className={cn('h-full rounded-full transition-[width] duration-500', over ? 'bg-destructive' : 'bg-primary')} style={{ width: `${activityBarPercent(used, limit, busiest)}%` }} />
      </div>
      <div className="flex flex-wrap items-center justify-between gap-x-2 text-xs text-muted-foreground">
        <span className="tabular-nums">{limit ? t('plugins.activityBudgetUsed', { used, limit }) : t('plugins.activityNoBudget', { used })}</span>
        {limit > 0 && <span className={cn('font-medium', over ? 'text-destructive' : '')}>{t('plugins.activityRemaining', { count: Math.max(0, remaining) })}</span>}
      </div>
    </div>
  )
}

// BPSActivityPanel is the live per-account activity table: in flight against
// the cap and successful requests against the budget as capacity bars, busy
// accounts first (server order), paginated. It polls every 2.5 seconds.
function BPSActivityPanel({ plugin }: { plugin: TransportPlugin }) {
  const { t } = useTranslation()
  const [data, setData] = useState<BPSActivity | null>(null)
  const [fetchedAt, setFetchedAt] = useState(() => Date.now())
  const [now, setNow] = useState(() => Date.now())
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = usePersistedPageSize('bps_activity', 20, DEFAULT_PAGE_SIZE_OPTIONS)
  useEffect(() => {
    let active = true
    const load = () => api.getBPSActivity(plugin.id)
      .then((res) => { if (active) { setData(res); setFetchedAt(Date.now()) } })
      .catch(() => undefined)
    void load()
    const refresh = window.setInterval(() => void load(), BPS_ACTIVITY_REFRESH_MS)
    const tick = window.setInterval(() => setNow(Date.now()), 1000)
    return () => { active = false; window.clearInterval(refresh); window.clearInterval(tick) }
  }, [plugin.id])
  const accounts = data?.accounts ?? []
  const totalPages = Math.max(1, Math.ceil(accounts.length / pageSize))
  const currentPage = Math.min(page, totalPages)
  const visible = accounts.slice((currentPage - 1) * pageSize, currentPage * pageSize)
  const busiestInFlight = Math.max(0, ...accounts.map((account) => account.in_flight))
  const busiestSucceeded = Math.max(0, ...accounts.map((account) => account.succeeded))
  const windowLabel = data ? formatWindowLabel(data.window_seconds) : ''

  const stateLabel = (account: BPSActivityAccount) => {
    switch (account.state) {
      case 'policy_blocked':
        return account.tiers ? t('plugins.dashStatePolicyTier', { tier: account.tier, tiers: account.tiers }) : t('plugins.dashStates.policy_blocked')
      default:
        return t(`plugins.dashStates.${account.state}`)
    }
  }

  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <h3 className="text-xs font-medium text-muted-foreground">{t('plugins.activityTitle')}</h3>
        <span className="text-[11px] text-muted-foreground">{t('plugins.activityNote', { window: windowLabel })}</span>
      </div>
      {data && accounts.length === 0 ? (
        <p className="text-sm text-muted-foreground">{t('plugins.dashNoAccounts')}</p>
      ) : (
        <>
          <div className="data-table-shell">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>{t('plugins.dashAccounts')}</TableHead>
                  <TableHead>{t('plugins.activityInFlight')}</TableHead>
                  <TableHead>{t('plugins.activityBudget', { window: windowLabel })}</TableHead>
                  <TableHead className="text-right">{t('plugins.activityAttempts')}</TableHead>
                  <TableHead>{t('plugins.activityLastRequestColumn')}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {visible.map((account) => {
                  const last = secondsSince(account.last_request_at, now)
                  const nextProbe = secondsUntil(account.next_probe_at, now)
                  return (
                    <TableRow key={account.account_id}>
                      <TableCell className="max-w-[12rem] space-y-1 sm:max-w-[16rem]">
                        <div className="flex min-w-0 items-center gap-2">
                          <span className="truncate text-sm font-medium">{account.name || `#${account.account_id}`}</span>
                          {account.in_flight > 0 && <span className="size-2 shrink-0 animate-pulse rounded-full bg-primary" aria-hidden />}
                        </div>
                        <div className="flex flex-col items-start gap-1 whitespace-normal text-[11px] text-muted-foreground">
                          <Badge variant={account.state === 'active' ? 'secondary' : account.state === 'policy_blocked' ? 'destructive' : 'outline'} className={BPS_STATE_BADGE_CLASSES[account.state]}>{stateLabel(account)}</Badge>
                          {account.state === 'policy_blocked' && account.elapsed_seconds !== undefined && (
                            <span>{t('plugins.activityBlockedFor', { elapsed: formatBlockDuration(liveElapsedSeconds(account.elapsed_seconds, fetchedAt, now)) })}</span>
                          )}
                          {account.state === 'rate_cooling' && activeUntil(account.cooling_until, now) && (
                            <span>{t('plugins.activityCoolingUntil', { time: formatBeijingTime(account.cooling_until).slice(5) })}</span>
                          )}
                          {hasTime(account.next_probe_at) && <span>{nextProbe > 0 ? t('plugins.activityNextProbe', { in: formatBlockDuration(nextProbe) }) : t('plugins.policyProbeDue')}</span>}
                        </div>
                      </TableCell>
                      <TableCell>
                        <InFlightBar value={account.in_flight} limit={account.max_concurrency} busiest={busiestInFlight} label={t('plugins.activityInFlight')} />
                      </TableCell>
                      <TableCell>
                        <BudgetBar used={account.succeeded} limit={account.budget} busiest={busiestSucceeded} label={t('plugins.activityBudget', { window: windowLabel })} />
                      </TableCell>
                      <TableCell className="text-right font-mono text-xs tabular-nums">{account.attempts}</TableCell>
                      <TableCell className="text-xs text-muted-foreground">
                        {last === undefined ? t('plugins.activityNoRequest') : t('plugins.activityLastRequest', { ago: formatBlockDuration(last) })}
                      </TableCell>
                    </TableRow>
                  )
                })}
              </TableBody>
            </Table>
          </div>
          <Pagination
            page={currentPage}
            totalPages={totalPages}
            onPageChange={setPage}
            totalItems={accounts.length}
            pageSize={pageSize}
            onPageSizeChange={(size) => { setPageSize(size); setPage(1) }}
            pageSizeOptions={DEFAULT_PAGE_SIZE_OPTIONS}
          />
        </>
      )}
    </div>
  )
}

// PolicyBlocks shows which accounts the BPS usage policy is blocking, for how
// long (ticking live), their tier and probes, plus the block history and
// per-account totals, including recovery times.
function PolicyBlocks({ plugin }: { plugin: TransportPlugin }) {
  const { t } = useTranslation()
  const [data, setData] = useState<BPSPolicyBlocksResponse | null>(null)
  const [fetchedAt, setFetchedAt] = useState(() => Date.now())
  const [now, setNow] = useState(() => Date.now())
  const [error, setError] = useState('')
  useEffect(() => {
    let active = true
    const load = () => api.getPluginPolicyBlocks(plugin.id)
      .then((res) => { if (active) { setData(res); setFetchedAt(Date.now()); setError('') } })
      .catch((err) => { if (active) setError(getErrorMessage(err)) })
    void load()
    const refresh = window.setInterval(() => void load(), POLICY_BLOCKS_REFRESH_MS)
    const tick = window.setInterval(() => setNow(Date.now()), 1000)
    return () => { active = false; window.clearInterval(refresh); window.clearInterval(tick) }
  }, [plugin.id])
  const accountLabel = (id: number, name: string) => name || `#${id}`

  return (
    <Section title={t('plugins.policyBlocks')} description={t('plugins.policyBlocksDesc')}>
      {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
      <div className="space-y-2">
        <h3 className="text-xs font-medium text-muted-foreground">{t('plugins.policyBlocksActive', { count: data?.active.length ?? 0 })}</h3>
        {data && data.active.length === 0 ? (
          <p className="text-sm text-muted-foreground">{t('plugins.policyBlocksNone')}</p>
        ) : (
          <div className="overflow-x-auto">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>{t('plugins.policyAccount')}</TableHead>
                  <TableHead>{t('plugins.policyBlockedSince')}</TableHead>
                  <TableHead>{t('plugins.policyElapsed')}</TableHead>
                  <TableHead>{t('plugins.policyTierColumn')}</TableHead>
                  <TableHead>{t('plugins.policyLastProbe')}</TableHead>
                  <TableHead>{t('plugins.policyNextProbe')}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {(data?.active ?? []).map((block) => {
                  const next = secondsUntil(block.next_probe_at, now)
                  return (
                    <TableRow key={block.id}>
                      <TableCell className="text-sm">{accountLabel(block.account_id, block.name)}</TableCell>
                      <TableCell className="whitespace-nowrap text-xs">{hasTime(block.blocked_at) ? formatBeijingTime(block.blocked_at) : '—'}</TableCell>
                      <TableCell className="whitespace-nowrap font-mono text-xs">{hasTime(block.blocked_at) ? formatBlockDuration(liveElapsedSeconds(block.elapsed_seconds, fetchedAt, now)) : '—'}</TableCell>
                      <TableCell className="text-xs">{block.tiers ? `${block.tier}/${block.tiers}` : block.tier}</TableCell>
                      <TableCell className="text-xs">{block.last_probe_result ? t('plugins.policyProbeCount', { result: block.last_probe_result, count: block.probe_count }) : '—'}</TableCell>
                      <TableCell className="whitespace-nowrap text-xs">{hasTime(block.next_probe_at) ? (next > 0 ? formatBlockDuration(next) : t('plugins.policyProbeDue')) : '—'}</TableCell>
                    </TableRow>
                  )
                })}
              </TableBody>
            </Table>
          </div>
        )}
      </div>
      {data && data.totals.length > 0 && (
        <div className="space-y-2">
          <h3 className="text-xs font-medium text-muted-foreground">{t('plugins.policyTotals')}</h3>
          <div className="overflow-x-auto">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>{t('plugins.policyAccount')}</TableHead>
                  <TableHead>{t('plugins.policyTimesBlocked')}</TableHead>
                  <TableHead>{t('plugins.policyTotalBlocked')}</TableHead>
                  <TableHead>{t('plugins.policyLongest')}</TableHead>
                  <TableHead>{t('plugins.policyRecovered')}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {data.totals.map((total) => (
                  <TableRow key={total.account_id}>
                    <TableCell className="text-sm">{accountLabel(total.account_id, total.name)}</TableCell>
                    <TableCell className="text-xs">{total.times_blocked}</TableCell>
                    <TableCell className="font-mono text-xs">{formatBlockDuration(total.total_blocked_seconds)}</TableCell>
                    <TableCell className="font-mono text-xs">{formatBlockDuration(total.longest_block_seconds)}</TableCell>
                    <TableCell className="text-xs">{total.recovered}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        </div>
      )}
      {data && data.history.length > 0 && (
        <div className="space-y-2">
          <h3 className="text-xs font-medium text-muted-foreground">{t('plugins.policyHistory')}</h3>
          <ul className="divide-y divide-border text-xs">
            {data.history.map((block) => (
              <li key={block.id} className="flex flex-wrap items-center justify-between gap-2 py-2">
                <span className="text-sm">{accountLabel(block.account_id, block.name)}</span>
                <span className="text-muted-foreground">
                  {t('plugins.policyHistoryEntry', {
                    blocked: formatBeijingTime(block.blocked_at),
                    cleared: block.cleared_at ? formatBeijingTime(block.cleared_at) : '—',
                    duration: formatBlockDuration(block.duration_seconds),
                    probes: block.probe_count,
                    tier: block.tier,
                  })}
                </span>
              </li>
            ))}
          </ul>
        </div>
      )}
    </Section>
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
                    {plugin.id === 'bps' && ` · ${account.enabled === false ? t('plugins.accountDisabled') : account.codex_bps_active ? t('accounts.bps.activeNow') : t('accounts.bps.inactiveNow')}`}
                  </span>
                  {account.enabled !== false && <PluginAccountStatusLine status={statuses.get(account.id)} />}
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

// PluginAccountStatusLine shows an account's plugin-scoped cooldown. Only a
// real cooldown still running shows as one: paused or forced-off accounts
// report no (or a zero) cooling time and get no cooling line.
function PluginAccountStatusLine({ status }: { status?: PluginAccountStatus }) {
  const { t } = useTranslation()
  const now = Date.now()
  const models = Object.entries(status?.models_unavailable ?? {}).filter(([, until]) => activeUntil(until, now))
  const cooling = activeUntil(status?.cooling_until, now) ? status?.cooling_until : undefined
  const capped = Boolean(status?.max_concurrency) || Boolean(status?.budget) || Boolean(status?.probe_pending) || Boolean(status?.last_probe_result)
  if (!status || (!cooling && !status.policy_strikes && !status.policy_tier && models.length === 0 && !capped)) return null
  return (
    <span className="mt-1 block space-y-0.5 text-xs text-amber-600 dark:text-amber-400">
      {Boolean(cooling || status.policy_strikes) && (
        <span className="block">
          {cooling
            ? t('plugins.coolingUntil', { time: formatBeijingTime(cooling), reason: t(pluginCoolingReasonKey(status.reason), { defaultValue: status.reason ?? '' }) })
            : t('plugins.policyStrikes', { count: status.policy_strikes })}
        </span>
      )}
      {Boolean(status.policy_tier) && <span className="block">{t('plugins.policyTier', { tier: status.policy_tier, tiers: status.policy_tiers })}</span>}
      {status.probe_pending && !cooling && <span className="block">{t('plugins.probePending')}</span>}
      {status.last_probe_result && hasTime(status.last_probe) && (
        <span className="block text-muted-foreground">{t('plugins.lastProbe', { time: formatBeijingTime(status.last_probe), result: status.last_probe_result })}</span>
      )}
      {Boolean(status.max_concurrency) && <span className="block text-muted-foreground">{t('plugins.inFlight', { current: status.in_flight ?? 0, max: status.max_concurrency })}</span>}
      {Boolean(status.budget) && (
        <span className={(status.budget_used ?? 0) >= (status.budget ?? 0) ? 'block' : 'block text-muted-foreground'}>
          {t('plugins.budgetUsed', { used: status.budget_used ?? 0, budget: status.budget })}
        </span>
      )}
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
