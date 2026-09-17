import { useTranslation } from 'react-i18next'
import { Badge } from '@/components/ui/badge'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import type { UsageLog } from '../types'

/**
 * Codex 客户端窗口号：同一台机器上开的第几个 Codex 窗口，用来把同一账号下
 * 并发的多个会话分开看。历史行没有这一列，畸形值也一律不显示——宁可空着，
 * 也不要在表里放一个来路不明的数字让人当成别的编号。
 */
export function UsageWindowNumberBadge({ log }: { log: UsageLog }) {
  const { t } = useTranslation()
  const windowNumber = log.window_number
  if (!windowNumber || !/^\d+$/.test(windowNumber)) return null

  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <Badge
          variant="outline"
          tabIndex={0}
          aria-label={`${t('usage.windowNumber')} ${windowNumber}`}
          className="cursor-default border-transparent bg-emerald-500/12 px-1.5 py-0 font-mono text-[11px] tabular-nums text-emerald-700 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring dark:bg-emerald-500/20 dark:text-emerald-300"
        >
          {windowNumber}
        </Badge>
      </TooltipTrigger>
      <TooltipContent>
        <span className="font-mono tabular-nums">{t('usage.windowNumberHint', { n: windowNumber })}</span>
      </TooltipContent>
    </Tooltip>
  )
}

export default UsageWindowNumberBadge
