import { Badge } from '@/components/ui/badge'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import type { UsageLog } from '@/types'

export function UsageWindowNumberBadge({ log }: { log: UsageLog }) {
  const bpsIteration = log.bps_agent_iteration
  if (bpsIteration && /^[1-9]\d*$/.test(bpsIteration)) {
    const label = `BPS agent_iteration: ${bpsIteration}`
    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <Badge
            variant="outline"
            tabIndex={0}
            aria-label={label}
            className="cursor-default border-transparent bg-amber-500/12 px-1.5 py-0 font-mono text-[11px] tabular-nums text-amber-700 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring dark:bg-amber-500/20 dark:text-amber-300"
          >
            {bpsIteration}
          </Badge>
        </TooltipTrigger>
        <TooltipContent><span className="font-mono tabular-nums">{label}</span></TooltipContent>
      </Tooltip>
    )
  }
  const original = log.window_number_original
  if (original === undefined || !/^\d+$/.test(original)) return null
  const outbound = log.window_number_outbound
  // Before account selection no outbound rewrite has occurred. For an attempted
  // upstream request, missing capture data cannot establish that it was unchanged.
  if (!outbound && log.account_id > 0) return null
  if (outbound && !/^\d+$/.test(outbound)) return null
  const changed = outbound !== undefined && /^\d+$/.test(outbound) && outbound !== original
  const text = changed ? `${original} → ${outbound}` : original

  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <Badge
          variant="outline"
          tabIndex={0}
          aria-label={text}
          className={`cursor-default border-transparent px-1.5 py-0 font-mono text-[11px] tabular-nums focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring ${changed
            ? 'bg-amber-500/12 text-amber-700 dark:bg-amber-500/20 dark:text-amber-300'
            : 'bg-emerald-500/12 text-emerald-700 dark:bg-emerald-500/20 dark:text-emerald-300'}`}
        >
          {changed ? outbound : original}
        </Badge>
      </TooltipTrigger>
      <TooltipContent><span className="font-mono tabular-nums">{text}</span></TooltipContent>
    </Tooltip>
  )
}
