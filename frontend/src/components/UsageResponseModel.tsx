import { useTranslation } from 'react-i18next'
import { cn } from '../lib/utils'
import type { UsageLog } from '../types'

function normalizeModelVariant(model: string): string {
  return model.trim().toLowerCase()
    .replace(/-latest$/, '')
    .replace(/-\d{4}-\d{2}-\d{2}$/, '')
    .replace(/-\d{8}$/, '')
}

/**
 * 上游自报的模型。上游没声明就什么都不渲染——这一行的意义是「对照」，
 * 补一个占位符反而会让整张表多出一行永远为空的噪音。
 *
 * 所有自报模型都保留；不一致时区分疑似变体（琥珀）与其他差异（橙色）。
 * 优先使用后端审计结果；旧记录没有审计标记时保留大小写不敏感的本地比对。
 */
export default function UsageResponseModel({ log }: { log: UsageLog }) {
  const { t } = useTranslation()
  const upstreamModel = log.upstream_response_model
  if (!upstreamModel) return null

  // 请求侧压根没有模型名时不标琥珀：那不是「上游换了模型」，只是没东西可比。
  const requestedModel = (log.effective_model || log.model || '').trim()
  const differs = !!requestedModel && (log.upstream_model_mismatch ?? (upstreamModel.trim().toLowerCase() !== requestedModel.toLowerCase()))
  const variant = differs && normalizeModelVariant(requestedModel) === normalizeModelVariant(upstreamModel)
  const titleLines = [
    t('usage.responseModelHint'),
    `${t('usage.requestedModel')}: ${log.model || '-'}`,
    `${t('usage.sentUpstreamModel')}: ${requestedModel || '-'}`,
    `${t('usage.upstreamResponseModel')}: ${upstreamModel}`,
  ]
  const tier = (log.billing_service_tier || log.service_tier || '').trim().toLowerCase()
  if (differs && ['fast', 'priority', 'ultrafast'].includes(tier)) {
    titleLines.push(t('usage.modelMismatchFastTierHint'))
  }

  return (
    <div
      className={cn(
        'basis-full min-w-0 break-all text-[11px] text-muted-foreground',
        differs && (variant ? 'text-amber-700 dark:text-amber-300' : 'text-orange-700 dark:text-orange-300'),
      )}
      title={titleLines.join('\n')}
    >
      ↳ {t('usage.responseModel')}: {upstreamModel}
      {differs && (
        <span className={cn(
          'ml-1 inline-flex rounded px-1 py-px text-[10px] font-medium ring-1 ring-inset',
          variant ? 'bg-amber-500/10 ring-amber-500/30' : 'bg-orange-500/10 ring-orange-500/30',
        )}>
          {variant ? t('usage.modelVariant') : t('usage.modelMismatch')}
        </span>
      )}
    </div>
  )
}
