import { useTranslation } from 'react-i18next'
import type { UsageLog } from '../types'
import { Tooltip, TooltipContent, TooltipTrigger } from './ui/tooltip'

export function UsageTurnState({ log, onClick }: { log: UsageLog; onClick: () => void }) {
  const { t } = useTranslation()
  const length = log.turn_state_length
  const label = length == null ? t('usage.turnState.notRecorded')
    : length === 0 ? `${t('usage.turnState.missing')} · ${t('usage.turnState.characters', { count: 0 })}` : t('usage.turnState.characters', { count: length })
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <button type="button" onClick={onClick}
          aria-label={`Turn-State: ${label}. ${t('usage.turnState.clickFilter')}`}
          className={`whitespace-nowrap rounded text-xs tabular-nums hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring ${length != null && length > 0 ? 'text-foreground' : 'text-muted-foreground'}`}>
          {label}
        </button>
      </TooltipTrigger>
      <TooltipContent className="max-w-xs">
        <p>{t('usage.turnState.hint')}</p>
        {length == null ? <p>{t('usage.turnState.notRecordedHint')}</p>
          : length === 0 ? <p>{t('usage.turnState.missingHint')}</p>
          : <p>{t('usage.turnState.characters', { count: length })}{log.turn_state_decoded_bytes != null
            ? ` · ${t('usage.turnState.decodedBytes', { count: log.turn_state_decoded_bytes })}` : ''}</p>}
        <p>{t('usage.turnState.clickFilter')}</p>
      </TooltipContent>
    </Tooltip>
  )
}
