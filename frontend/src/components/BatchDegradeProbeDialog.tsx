import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import Modal from './Modal'
import { api } from '../api'
import { getErrorMessage } from '../utils/error'
import { Button } from '@/components/ui/button'
import { SegmentedPillGroup } from '@/components/ui/segmented-pill-group'

export interface BatchDegradeProbeResult {
  queued: number
  skipped: number
}

// BatchDegradeProbeDialog queues pelican degradation checks of one route for
// the selected accounts; the server runs at most degrade_probe_max_concurrent
// at a time and reports accounts it skipped (queued already, or a native
// route that is not explicitly on).
export default function BatchDegradeProbeDialog({ show, ids, onClose, onDone }: {
  show: boolean
  ids: number[]
  onClose: () => void
  onDone: (result: BatchDegradeProbeResult) => void
}) {
  const { t } = useTranslation()
  const [route, setRoute] = useState<'bps' | 'native'>('bps')
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState('')
  useEffect(() => {
    if (show) {
      setRoute('bps')
      setError('')
    }
  }, [show])
  const submit = async () => {
    setSubmitting(true)
    setError('')
    try {
      const result = await api.startDegradeProbes('bps', ids, route)
      onDone({ queued: result.queued, skipped: result.skipped.length })
    } catch (err) {
      setError(getErrorMessage(err))
    } finally {
      setSubmitting(false)
    }
  }
  return (
    <Modal
      show={show}
      title={t('accounts.batchDegrade.title')}
      contentClassName="sm:max-w-[520px]"
      onClose={() => { if (!submitting) onClose() }}
      footer={
        <>
          <Button type="button" variant="outline" onClick={onClose} disabled={submitting}>{t('common.cancel')}</Button>
          <Button type="button" onClick={() => void submit()} disabled={submitting || ids.length === 0}>
            {submitting ? t('common.saving') : t('accounts.batchDegrade.start', { count: ids.length })}
          </Button>
        </>
      }
    >
      <div className="space-y-4">
        <p className="rounded-lg border border-border bg-muted/20 p-3 text-sm text-muted-foreground">{t('accounts.batchDegrade.desc', { count: ids.length })}</p>
        <SegmentedPillGroup
          label={t('accounts.batchDegrade.route')}
          value={route}
          onChange={setRoute}
          options={[{ value: 'bps', label: 'BPS' }, { value: 'native', label: t('accounts.batchDegrade.native') }]}
          disabled={submitting}
        />
        {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
      </div>
    </Modal>
  )
}
