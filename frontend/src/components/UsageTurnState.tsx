import { useTranslation } from 'react-i18next'
import { Button } from '@/components/ui/button'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import { cn } from '../lib/utils'
import type { UsageLog } from '../types'

export type UsageTurnStateKind = 'received' | 'missing' | 'not_recorded'

/**
 * usageTurnStateKind 把一行折成三态,列筛选和点击筛选共用同一套判定,
 * 免得表里显示「未获取」点下去却筛到别的集合。
 */
export function usageTurnStateKind(source: { turn_state_length?: number | null }): UsageTurnStateKind {
  const length = source.turn_state_length
  if (length == null) return 'not_recorded'
  return length > 0 ? 'received' : 'missing'
}

const ECHO_CLASSES = ['none', 'same', 'cross', 'unknown', 'substitute'] as const

/** 后端新增分类时前端不至于渲染出 `usage.turnState.echo.xxx` 这种裸 key,统一落到 unknown。 */
export function usageTurnStateEchoKey(echo: string | undefined | null): string | null {
  if (!echo) return null
  return (ECHO_CLASSES as readonly string[]).includes(echo) ? echo : 'unknown'
}

/**
 * 本次请求真正带给上游的 turn-state token 有多长。显示的是字符数而不是一个
 * 「有/无」徽章:实测唯一健康的官号是 292 字符,其余九个号一律 312——长度本身
 * 就是账号被降级的标记,折成布尔就把这条信息丢了。
 *
 * 长度和回带分类都没记的行(历史行,以及压根不走 turn-state 的渠道)什么都不渲染:
 * 满屏的「未记录」只会把真正有数字的那几行盖掉。要看这类行用筛选器。
 */
export function UsageTurnState({ log, onClick }: { log: UsageLog; onClick: () => void }) {
  const { t } = useTranslation()
  const length = log.turn_state_length
  const echo = usageTurnStateEchoKey(log.turn_state_echo)
  if (length == null && !echo) return null

  const kind = usageTurnStateKind(log)
  const label = kind === 'received'
    ? t('usage.turnState.characters', { count: length as number })
    : kind === 'missing'
      ? t('usage.turnState.missing')
      : t('usage.turnState.notRecorded')

  const echoLine = echo
    ? t(log.turn_state_stripped ? 'usage.turnState.echoLineStripped' : 'usage.turnState.echoLine', {
        value: t(`usage.turnState.echo.${echo}`),
      })
    : null

  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <Button
          variant="link"
          size="xs"
          onClick={onClick}
          aria-label={`turn-state: ${label}. ${t('usage.turnState.clickFilter')}`}
          className={cn(
            'h-auto px-0 py-0 text-[11px] font-normal tabular-nums no-underline hover:underline',
            kind === 'received' && 'text-foreground',
            kind === 'missing' && 'text-amber-600 dark:text-amber-400',
            kind === 'not_recorded' && 'text-muted-foreground',
          )}
        >
          {label}
        </Button>
      </TooltipTrigger>
      <TooltipContent className="max-w-xs">
        <p>{t('usage.turnState.hint')}</p>
        {echoLine ? <p className="mt-1">{echoLine}</p> : null}
        <p className="mt-1 text-muted-foreground">{t('usage.turnState.clickFilter')}</p>
      </TooltipContent>
    </Tooltip>
  )
}

export default UsageTurnState
