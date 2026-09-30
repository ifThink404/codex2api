import { createContext, useContext, useEffect, useMemo, useState, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import { api } from '../api'
import type { PluginAccountStatus } from '../types'
import { activeUntil, hasTime, pluginCoolingReasonKey } from '../lib/transportPlugins'
import { formatBeijingTime } from '../utils/time'
import { cn } from '../lib/utils'

// PluginAccountStatusLine shows an account's plugin-scoped state. Only a real
// cooldown still running shows as one: paused or forced-off accounts report
// no (or a zero) cooling time and get no cooling line. compact (the Accounts
// list) keeps the lines that explain why BPS is not serving the account and
// leaves out in-flight counts and probe history.
export function PluginAccountStatusLine({ status, compact = false }: { status?: PluginAccountStatus; compact?: boolean }) {
  const { t } = useTranslation()
  const now = Date.now()
  const models = Object.entries(status?.models_unavailable ?? {}).filter(([, until]) => activeUntil(until, now))
  const cooling = activeUntil(status?.cooling_until, now) ? status?.cooling_until : undefined
  const budgetExhausted = Boolean(status?.budget) && (status?.budget_used ?? 0) >= (status?.budget ?? 0)
  const details = compact
    ? budgetExhausted
    : Boolean(status?.max_concurrency) || Boolean(status?.budget) || Boolean(status?.probe_pending) || Boolean(status?.last_probe_result)
  if (!status || (!cooling && !status.policy_strikes && !status.policy_tier && models.length === 0 && !details && !(compact && status.probe_pending))) return null
  return (
    <span className={cn('block space-y-0.5 text-amber-600 dark:text-amber-400', compact ? 'w-full min-w-0 basis-full whitespace-normal break-words text-[11px] leading-snug' : 'mt-1 text-xs')}>
      {Boolean(cooling || status.policy_strikes) && (
        <span className="block">
          {cooling
            ? t('plugins.coolingUntil', { time: formatBeijingTime(cooling), reason: t(pluginCoolingReasonKey(status.reason), { defaultValue: status.reason ?? '' }) })
            : t('plugins.policyStrikes', { count: status.policy_strikes })}
        </span>
      )}
      {Boolean(status.policy_tier) && <span className="block">{t('plugins.policyTier', { tier: status.policy_tier, tiers: status.policy_tiers })}</span>}
      {status.probe_pending && !cooling && <span className="block">{t('plugins.probePending')}</span>}
      {!compact && status.last_probe_result && hasTime(status.last_probe) && (
        <span className="block text-muted-foreground">{t('plugins.lastProbe', { time: formatBeijingTime(status.last_probe), result: status.last_probe_result })}</span>
      )}
      {!compact && Boolean(status.max_concurrency) && <span className="block text-muted-foreground">{t('plugins.inFlight', { current: status.in_flight ?? 0, max: status.max_concurrency })}</span>}
      {Boolean(status.budget) && (!compact || budgetExhausted) && (
        <span className={budgetExhausted ? 'block' : 'block text-muted-foreground'}>
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

// BPSAccountStatus is the compact BPS state of one account in the Accounts
// list (nothing while BPS serves it normally).
export function BPSAccountStatus({ accountId }: { accountId: number }) {
  return <PluginAccountStatusLine status={useContext(BPSAccountStatusContext).get(accountId)} compact />
}
