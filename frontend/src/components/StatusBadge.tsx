import { Badge } from '@/components/ui/badge'
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from '@/components/ui/tooltip'
import { useTranslation } from 'react-i18next'

interface StatusBadgeProps {
  status?: string | null
  detail?: string | null
  errorMessage?: string | null
}

const statusConfig: Record<string, { variant: 'default' | 'secondary' | 'destructive' | 'outline'; dotColor: string; hintKey?: string }> = {
  active: { variant: 'default', dotColor: 'bg-emerald-500' },
  ready: { variant: 'default', dotColor: 'bg-emerald-500' },
  cooldown: { variant: 'secondary', dotColor: 'bg-amber-500' },
  api_upstream_unavailable: { variant: 'secondary', dotColor: 'bg-amber-500' },
  payment_required: { variant: 'secondary', dotColor: 'bg-amber-500', hintKey: 'status.paymentRequiredHint' },
  payment_required_unknown: { variant: 'secondary', dotColor: 'bg-amber-500', hintKey: 'status.paymentRequiredHint' },
  credential_refresh: { variant: 'secondary', dotColor: 'bg-blue-500', hintKey: 'status.runtimeRestrictionHint' },
  version_required: { variant: 'secondary', dotColor: 'bg-amber-500', hintKey: 'status.runtimeRestrictionHint' },
  usage_limit: { variant: 'secondary', dotColor: 'bg-yellow-500', hintKey: 'status.runtimeRestrictionHint' },
  forbidden: { variant: 'secondary', dotColor: 'bg-amber-500', hintKey: 'status.runtimeRestrictionHint' },
  quality_degraded: { variant: 'secondary', dotColor: 'bg-amber-500', hintKey: 'status.runtimeRestrictionHint' },
  grok_empty_stream: { variant: 'secondary', dotColor: 'bg-amber-500', hintKey: 'status.runtimeRestrictionHint' },
  rate_limited: { variant: 'secondary', dotColor: 'bg-yellow-500' },
  responses_rate_limited: { variant: 'secondary', dotColor: 'bg-yellow-500' },
  rate_limited_5h: { variant: 'secondary', dotColor: 'bg-yellow-500' },
  rate_limited_7d: { variant: 'secondary', dotColor: 'bg-yellow-500' },
  usage_exhausted: { variant: 'secondary', dotColor: 'bg-yellow-500' },
  usage_limited: { variant: 'secondary', dotColor: 'bg-yellow-500' },
  quota_paused: { variant: 'secondary', dotColor: 'bg-yellow-500' },
  overload_paused: { variant: 'secondary', dotColor: 'bg-orange-500' },
  unauthorized: { variant: 'destructive', dotColor: 'bg-red-500' },
  error: { variant: 'destructive', dotColor: 'bg-red-400' },
  refreshing: { variant: 'secondary', dotColor: 'bg-blue-500 animate-pulse' },
  paused: { variant: 'outline', dotColor: 'bg-blue-500' },
  unsampled: { variant: 'outline', dotColor: 'bg-slate-400' },
}

export default function StatusBadge({ status, detail, errorMessage }: StatusBadgeProps) {
  const { t } = useTranslation()
  const key = status ?? 'unknown'
  const config = statusConfig[key] ?? { variant: 'outline' as const, dotColor: 'bg-gray-400' }
  const trimmedError = errorMessage?.trim() ?? ''
  const recoveringAPI = key === 'api_upstream_unavailable'
  const unsampled = key === 'unsampled'
  const hintKey = statusConfig[key]?.hintKey
  const reasonHint = hintKey ? t(hintKey, { reason: t(`status.${key}`) }) : ''
  const showErrorTooltip = key === 'unauthorized' || key === 'error' || recoveringAPI || unsampled || Boolean(reasonHint)

  const badge = (
    <Badge
      variant={config.variant}
      className={`gap-1.5 text-[13px] ${showErrorTooltip ? 'cursor-help ring-1 ring-inset ring-current/10' : ''}`}
    >
      <span className={`size-1.5 rounded-full ${config.dotColor}`} />
      {t(`status.${key}`, { defaultValue: t('status.unknown', { defaultValue: key }) })}
      {detail && (
        <>
          <span className="h-3 w-px bg-current/20" />
          <span className="text-[11px] font-bold leading-none">{detail}</span>
        </>
      )}
    </Badge>
  )

  if (!showErrorTooltip) {
    return badge
  }

  const hint = recoveringAPI
    ? t('status.apiUpstreamUnavailableHint')
    : unsampled
      ? t('status.unsampledHint')
      : reasonHint
  const message = [hint, trimmedError].filter(Boolean).join('\n\n') || t('usage.statusErrorEmpty')

  return (
    <TooltipProvider>
      <Tooltip>
        <TooltipTrigger asChild>
          <span
            tabIndex={0}
            aria-label={message}
            className="inline-flex focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
          >
            {badge}
          </span>
        </TooltipTrigger>
        <TooltipContent
          side="right"
          sideOffset={8}
          className="max-w-[360px] rounded-lg border border-slate-700 bg-slate-950 px-3 py-2.5 text-xs text-slate-50 shadow-xl"
        >
          <div className="space-y-1.5">
            <div className="font-semibold text-slate-300">{unsampled || recoveringAPI || reasonHint ? t(`status.${key}`) : t('usage.statusErrorDetails')}</div>
            <div className="whitespace-pre-wrap break-words leading-relaxed text-slate-50">{message}</div>
          </div>
        </TooltipContent>
      </Tooltip>
    </TooltipProvider>
  )
}
