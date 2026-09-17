import { useTranslation } from 'react-i18next'
import { cn } from '../lib/utils'
import type { UsageLog } from '../types'

/**
 * 上游自报的模型。上游没声明就什么都不渲染——这一行的意义是「对照」，
 * 补一个占位符反而会让整张表多出一行永远为空的噪音。
 *
 * 与请求模型（优先取生效模型）不一致时标成琥珀色：模型被上游偷换是要人工介入的
 * 异常，静音显示等于没记。大小写不敏感，上游大小写写法不一致不算换模型。
 */
export default function UsageResponseModel({ log }: { log: UsageLog }) {
  const { t } = useTranslation()
  const upstreamModel = log.upstream_response_model
  if (!upstreamModel) return null

  const requestedModel = log.effective_model || log.model
  const differs = upstreamModel.toLowerCase() !== (requestedModel || '').toLowerCase()

  return (
    <div
      className={cn(
        'basis-full min-w-0 break-all text-[11px] text-muted-foreground',
        differs && 'text-amber-700 dark:text-amber-300',
      )}
      title={t('usage.responseModelHint')}
    >
      ↳ {t('usage.responseModel')}: {upstreamModel}
    </div>
  )
}
