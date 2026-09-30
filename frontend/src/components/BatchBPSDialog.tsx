import { useEffect, useId, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Cable } from 'lucide-react'
import Modal from './Modal'
import { api } from '../api'
import { getErrorMessage } from '../utils/error'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'
import {
  BATCH_KEEP,
  BPS_CONVERGENCE_MODES,
  BPS_PROFILES,
  batchBPSPayload,
  emptyBatchBPSForm,
  type BatchBPSForm,
} from '../lib/bpsAccount'

export interface BatchBPSResult {
  success: number
  failed: number
}

// BatchBPSDialog sets the BPS settings of every selected account at once:
// the per-account BPS switch (the plugin override), profile, convergence,
// native route, image trim and the route model scopes. Each field starts as
// "keep"; only the changed ones are sent. Accounts that cannot use BPS are
// skipped by the server and counted as failed.
export default function BatchBPSDialog({ show, ids, onClose, onDone }: {
  show: boolean
  ids: number[]
  onClose: () => void
  onDone: (result: BatchBPSResult) => void
}) {
  const { t } = useTranslation()
  const id = useId()
  const [form, setForm] = useState<BatchBPSForm>(emptyBatchBPSForm)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState('')
  useEffect(() => {
    if (show) {
      setForm(emptyBatchBPSForm())
      setError('')
    }
  }, [show])
  const payload = batchBPSPayload(form)
  const changes = Object.keys(payload).length
  const patch = (next: Partial<BatchBPSForm>) => setForm((prev) => ({ ...prev, ...next }))

  const submit = async () => {
    if (!changes || ids.length === 0) return
    setSubmitting(true)
    setError('')
    try {
      const result = await api.batchUpdateAccounts({ ids, ...payload })
      onDone({ success: result.success, failed: result.failed })
    } catch (err) {
      setError(getErrorMessage(err))
    } finally {
      setSubmitting(false)
    }
  }

  const keep = { value: BATCH_KEEP, label: t('accounts.batchBPS.keep') }
  const triState = (inherit: string) => [
    keep,
    { value: 'inherit', label: inherit },
    { value: 'on', label: t('accounts.bps.on') },
    { value: 'off', label: t('accounts.bps.off') },
  ]
  const selectField = (key: 'enabled' | 'native' | 'imageTrim' | 'profile' | 'convergence', label: string, options: Array<{ value: string; label: string }>) => (
    <div className="space-y-1.5">
      <label htmlFor={`${id}-${key}`} className="text-xs font-medium">{label}</label>
      <Select
        id={`${id}-${key}`}
        value={form[key]}
        onValueChange={(value) => patch({ [key]: value } as Partial<BatchBPSForm>)}
        options={options}
        disabled={submitting}
        aria-label={label}
      />
    </div>
  )
  const modelsField = (key: 'bpsModels' | 'nativeModels', label: string) => (
    <div className="space-y-1.5">
      <div className="flex items-center justify-between gap-2">
        <label htmlFor={`${id}-${key}`} className="text-xs font-medium">{label}</label>
        <Switch
          checked={form[key] !== null}
          onCheckedChange={(on) => patch({ [key]: on ? '' : null } as Partial<BatchBPSForm>)}
          disabled={submitting}
          aria-label={t('accounts.batchBPS.replaceScope', { field: label })}
        />
      </div>
      <Input
        id={`${id}-${key}`}
        value={form[key] ?? ''}
        onChange={(event) => patch({ [key]: event.target.value } as Partial<BatchBPSForm>)}
        placeholder={form[key] === null ? t('accounts.batchBPS.keep') : t('accounts.bps.modelsPlaceholder')}
        disabled={submitting || form[key] === null}
      />
    </div>
  )

  return (
    <Modal
      show={show}
      title={t('accounts.batchBPS.title')}
      contentClassName="sm:max-w-[640px]"
      onClose={() => { if (!submitting) onClose() }}
      footer={
        <>
          <Button type="button" variant="outline" onClick={onClose} disabled={submitting}>{t('common.cancel')}</Button>
          <Button type="button" onClick={() => void submit()} disabled={submitting || !changes}>
            {submitting ? t('common.saving') : t('accounts.batchBPS.apply', { count: changes })}
          </Button>
        </>
      }
    >
      <div className="space-y-4">
        <div className="rounded-lg border border-border bg-muted/20 p-3 text-sm text-muted-foreground">
          {t('accounts.batchBPS.desc', { count: ids.length })}
        </div>
        <div className="space-y-3 rounded-xl border border-border/70 bg-card p-4">
          <span className="flex items-center gap-2 text-sm font-semibold">
            <Cable className="size-4 text-primary" aria-hidden />
            {t('accounts.bps.title')}
          </span>
          {selectField('enabled', t('accounts.bps.enabled'), triState(t('accounts.bps.inheritPlugin')))}
          <p className="text-xs text-muted-foreground">{t('accounts.bps.enabledHelp')}</p>
          <div className="grid gap-3 sm:grid-cols-2">
            {selectField('profile', t('accounts.bps.profile'), [keep, ...BPS_PROFILES.map((value) => ({ value, label: t(`accounts.bps.profiles.${value}`) }))])}
            {selectField('convergence', t('accounts.bps.convergence'), [keep, ...BPS_CONVERGENCE_MODES.map((value) => ({ value, label: t(`accounts.bps.convergenceModes.${value}`) }))])}
            {selectField('native', t('accounts.bps.native'), triState(t('accounts.bps.inheritNative')))}
            {selectField('imageTrim', t('accounts.bps.imageTrim'), triState(t('accounts.bps.inheritImageTrim')))}
          </div>
          <div className="grid gap-3 sm:grid-cols-2">
            {modelsField('bpsModels', t('accounts.bps.bpsModels'))}
            {modelsField('nativeModels', t('accounts.bps.nativeModels'))}
          </div>
          <p className="text-xs text-muted-foreground">{t('accounts.batchBPS.scopeHint')}</p>
        </div>
        {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
      </div>
    </Modal>
  )
}
