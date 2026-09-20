import { useTranslation } from 'react-i18next'
import type { CodexUserAgentObservation } from '../types'
import { formatBeijingTime } from '../utils/time'

export default function CodexUAObservation({ observation }: { observation?: CodexUserAgentObservation }) {
  const { t } = useTranslation()
  if (!observation) return null
  const status = observation.status
  return (
    <div className="mt-1.5 space-y-0.5 font-sans text-[11px] leading-5 text-muted-foreground">
      <div>{t(`settings.codexUAObserved_${status}`, {
        count: observation.match_count,
        time: observation.last_seen_at ? formatBeijingTime(observation.last_seen_at) : '-',
      })}</div>
      {(status === 'matched' || status === 'unseen') && observation.version_pair_count > 0 ? (
        <div>{t('settings.codexUAVersionPairObserved', {
          count: observation.version_pair_count,
          time: observation.version_pair_last_seen_at ? formatBeijingTime(observation.version_pair_last_seen_at) : '-',
        })}</div>
      ) : null}
      {observation.warnings?.length ? (
        <div className="text-amber-600 dark:text-amber-400">
          {t('settings.codexUAWarnUnseen', { fields: observation.warnings.map((field) => t(`settings.codexUAWarn_${field}`)).join(' / ') })}
        </div>
      ) : null}
    </div>
  )
}
