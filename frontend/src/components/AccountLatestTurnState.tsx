import { useTranslation } from 'react-i18next'
import { usageTurnStateEchoKey, usageTurnStateKind } from './UsageTurnState'
import type { AccountLatestTurnStateInfo } from '../types'
import { formatRelativeTime } from '../utils/time'

/**
 * 账号健康条下面那一行小字:这个号最近一次请求真正带上去的 turn-state 有多长。
 * 长度是账号级的降级标记(健康号 292 字符、降级号 312),放在健康条旁边才能
 * 一眼看出「成功率没塌但这个号已经被降级了」。
 *
 * 后端没给样本(历史号、lite 列表)时整行不渲染,不占位也不显示占位符。
 */
export default function AccountLatestTurnState({ state }: { state?: AccountLatestTurnStateInfo | null }) {
  const { t } = useTranslation()
  if (!state) return null

  const kind = usageTurnStateKind(state)
  const parts: string[] = [
    kind === 'received'
      ? t('usage.turnState.characters', { count: state.turn_state_length as number })
      : kind === 'missing'
        ? t('usage.turnState.missing')
        : t('usage.turnState.notRecorded'),
  ]

  const echo = usageTurnStateEchoKey(state.turn_state_echo)
  if (echo) {
    parts.push(
      t(state.turn_state_stripped ? 'usage.turnState.echoLineStripped' : 'usage.turnState.echoLine', {
        value: t(`usage.turnState.echo.${echo}`),
      }),
    )
  }

  const age = formatRelativeTime(state.created_at, { variant: 'compact', fallback: '' })
  if (age) parts.push(age)

  const value = parts.join(' · ')
  const line = t('accounts.healthBarTurnState', { value })
  return (
    <div className="mt-1 truncate text-[11px] leading-snug text-muted-foreground" title={line}>
      {line}
    </div>
  )
}
