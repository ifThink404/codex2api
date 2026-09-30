import { createContext, useContext, useEffect, useMemo, useState, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import { api } from '../api'
import type { PluginAccountStatus } from '../types'
import { activeUntil, hasTime, pluginCoolingReasonKey } from '../lib/transportPlugins'
import { formatBeijingTime } from '../utils/time'
import { cn } from '../lib/utils'
import { bpsBadgeView, type BPSBadgeTone } from '../lib/bpsBadge'
import { Layers, ShieldAlert, Timer, TriangleAlert } from 'lucide-react'
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from '@/components/ui/tooltip'

// PluginAccountStatusLine shows an account's plugin-scoped state. Only a real
// cooldown still running shows as one: paused or forced-off accounts report
// no (or a zero) cooling time and get no cooling line.
export function PluginAccountStatusLine({ status, className }: { status?: PluginAccountStatus; className?: string }) {
  const { t } = useTranslation()
  const now = Date.now()
  const models = Object.entries(status?.models_unavailable ?? {}).filter(([, until]) => activeUntil(until, now))
  const cooling = activeUntil(status?.cooling_until, now) ? status?.cooling_until : undefined
  const capped = Boolean(status?.max_concurrency) || Boolean(status?.budget) || Boolean(status?.probe_pending) || Boolean(status?.last_probe_result)
  if (!status || (!cooling && !status.policy_strikes && !status.policy_tier && models.length === 0 && !capped && !status.bps_degraded)) return null
  return (
    <span className={cn('mt-1 block space-y-0.5 text-xs text-amber-600 dark:text-amber-400', className)}>
      {Boolean(cooling || status.policy_strikes) && (
        <span className="block">
          {cooling
            ? t('plugins.coolingUntil', { time: formatBeijingTime(cooling), reason: t(pluginCoolingReasonKey(status.reason), { defaultValue: status.reason ?? '' }) })
            : t('plugins.policyStrikes', { count: status.policy_strikes })}
        </span>
      )}
      {Boolean(status.policy_tier) && <span className="block">{t('plugins.policyTier', { tier: status.policy_tier, tiers: status.policy_tiers })}</span>}
      {status.probe_pending && !cooling && <span className="block">{t('plugins.probePending')}</span>}
      {status.bps_degraded_detail && <span className="block">{t('plugins.bpsDegradedDetail', { detail: status.bps_degraded_detail })}</span>}
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

const BPS_PLUGIN_ID = 'bps'
// The account-status endpoint takes at most 200 IDs per request.
const BPS_STATUS_BATCH = 200
const BPS_STATUS_REFRESH_MS = 30_000

const BPSAccountStatusContext = createContext<Map<number, PluginAccountStatus>>(new Map())

// BPSAccountStatusProvider loads the BPS plugin state of the listed accounts
// that BPS currently serves (the same data as the Plugins-page account list)
// and refreshes it every 30 seconds.
export function BPSAccountStatusProvider({ accounts, children }: { accounts: Array<{ id: number; codex_bps_active?: boolean }>; children: ReactNode }) {
  const ids = useMemo(() => accounts.filter((account) => account.codex_bps_active).map((account) => account.id).slice(0, BPS_STATUS_BATCH).join(','), [accounts])
  const [statuses, setStatuses] = useState<Map<number, PluginAccountStatus>>(() => new Map())
  useEffect(() => {
    if (!ids) {
      setStatuses(new Map())
      return undefined
    }
    let active = true
    const load = () => {
      api.getPluginAccountStatus(BPS_PLUGIN_ID, ids.split(',').map(Number))
        .then((res) => { if (active) setStatuses(new Map((res.accounts ?? []).map((item) => [item.account_id, item]))) })
        .catch(() => { if (active) setStatuses(new Map()) })
    }
    load()
    const timer = window.setInterval(load, BPS_STATUS_REFRESH_MS)
    return () => {
      active = false
      window.clearInterval(timer)
    }
  }, [ids])
  return <BPSAccountStatusContext.Provider value={statuses}>{children}</BPSAccountStatusContext.Provider>
}

const BPS_BADGE_TONES: Record<BPSBadgeTone, string> = {
  active: 'bg-emerald-50 text-emerald-700 ring-emerald-600/20 dark:bg-emerald-950 dark:text-emerald-400 dark:ring-emerald-400/20',
  cooling: 'bg-amber-50 text-amber-700 ring-amber-600/20 dark:bg-amber-950 dark:text-amber-400 dark:ring-amber-400/20',
  blocked: 'bg-rose-50 text-rose-700 ring-rose-600/20 dark:bg-rose-950 dark:text-rose-400 dark:ring-rose-400/20',
  degraded: 'bg-rose-50 text-rose-700 ring-rose-600/20 dark:bg-rose-950 dark:text-rose-400 dark:ring-rose-400/20',
}

const BPS_BADGE_ICONS: Record<BPSBadgeTone, typeof Layers> = { active: Layers, cooling: Timer, blocked: ShieldAlert, degraded: TriangleAlert }

// BPSStatusBadge is the one BPS indicator of an Accounts row / card: the
// "BPS" pill next to the status badge (upstream's look), toned by the plugin
// state, with the full plugin detail in its tooltip. Nothing when BPS does
// not serve the account.
export function BPSStatusBadge({ accountId, active, variant = 'row' }: { accountId: number; active?: boolean; variant?: 'row' | 'card' }) {
  const { t } = useTranslation()
  const status = useContext(BPSAccountStatusContext).get(accountId)
  if (!active) return null
  const view = bpsBadgeView(status, Date.now())
  const time = view.until ? formatBeijingTime(view.until).slice(11, 16) : ''
  let label = t('accounts.bpsBadge.active')
  if (view.tone === 'blocked') label = view.tier ? t('accounts.bpsBadge.blockedTier', { tier: view.tier, tiers: view.tiers ?? view.tier }) : t('accounts.bpsBadge.blocked')
  else if (view.tone === 'degraded') label = t('accounts.bpsBadge.degraded')
  else if (view.tone === 'cooling') label = view.cause === 'budget' ? t('accounts.bpsBadge.budget') : time ? t('accounts.bpsBadge.coolingUntil', { time }) : t('accounts.bpsBadge.cooling')
  const Icon = BPS_BADGE_ICONS[view.tone]
  const pill = (
    <span
      data-bps-tone={view.tone}
      className={cn(
        'inline-flex max-w-full cursor-help items-center gap-1 rounded-md font-medium ring-1 ring-inset',
        variant === 'card' ? 'px-[0.4375rem] py-1 text-[11px] leading-[1.2]' : 'px-1.5 py-0.5 text-[11px]',
        BPS_BADGE_TONES[view.tone],
      )}
    >
      <Icon className="size-3 shrink-0" aria-hidden />
      <span className="truncate tabular-nums">{label}</span>
    </span>
  )
  return (
    <TooltipProvider>
      <Tooltip>
        <TooltipTrigger asChild>{pill}</TooltipTrigger>
        <TooltipContent className="max-w-xs [&_.text-muted-foreground]:text-current [&_.text-muted-foreground]:opacity-75">
          <span className="block font-medium">{t('accounts.bpsBadge.title')}</span>
          <PluginAccountStatusLine status={status} className="text-current dark:text-current" />
        </TooltipContent>
      </Tooltip>
    </TooltipProvider>
  )
}
