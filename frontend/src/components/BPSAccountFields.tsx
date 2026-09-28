import { useId } from 'react'
import { useTranslation } from 'react-i18next'
import { Cable } from 'lucide-react'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'
import {
  BPS_CONVERGENCE_MODES,
  BPS_PROFILES,
  type BPSAccountForm,
  type BPSTriState,
} from '../lib/bpsAccount'

// Account-level controls of the BPS transport plugin. `compact` renders only
// the per-account switch (quick configuration); the edit dialog renders all.
export default function BPSAccountFields({
  form,
  onChange,
  disabled = false,
  active,
  compact = false,
}: {
  form: BPSAccountForm
  onChange: (patch: Partial<BPSAccountForm>) => void
  disabled?: boolean
  active?: boolean
  compact?: boolean
}) {
  const { t } = useTranslation()
  const id = useId()
  const triStateOptions = (inherit: string) => [
    { value: 'inherit', label: inherit },
    { value: 'on', label: t('accounts.bps.on') },
    { value: 'off', label: t('accounts.bps.off') },
  ]
  return (
    <div className="rounded-xl border border-border/70 bg-card p-4 space-y-3">
      <div className="flex items-center justify-between gap-3">
        <span className="flex items-center gap-2 text-sm font-semibold">
          <Cable className="size-4 text-primary" aria-hidden />
          {t('accounts.bps.title')}
        </span>
        {active !== undefined && (
          <span className="text-xs text-muted-foreground">
            {active ? t('accounts.bps.activeNow') : t('accounts.bps.inactiveNow')}
          </span>
        )}
      </div>
      <div className="space-y-1.5">
        <label htmlFor={`${id}-enabled`} className="text-xs font-medium">{t('accounts.bps.enabled')}</label>
        <Select
          id={`${id}-enabled`}
          value={form.enabled}
          onValueChange={value => onChange({ enabled: value as BPSTriState })}
          options={triStateOptions(t('accounts.bps.inheritPlugin'))}
          disabled={disabled}
          aria-label={t('accounts.bps.enabled')}
        />
        <p className="text-xs text-muted-foreground">{t('accounts.bps.enabledHelp')}</p>
      </div>
      {!compact && (
        <>
          <div className="grid gap-3 sm:grid-cols-2">
            <div className="space-y-1.5">
              <label htmlFor={`${id}-profile`} className="text-xs font-medium">{t('accounts.bps.profile')}</label>
              <Select
                id={`${id}-profile`}
                value={form.profile}
                onValueChange={value => onChange({ profile: value as BPSAccountForm['profile'] })}
                options={BPS_PROFILES.map(value => ({ value, label: t(`accounts.bps.profiles.${value}`) }))}
                disabled={disabled}
                aria-label={t('accounts.bps.profile')}
              />
            </div>
            <div className="space-y-1.5">
              <label htmlFor={`${id}-convergence`} className="text-xs font-medium">{t('accounts.bps.convergence')}</label>
              <Select
                id={`${id}-convergence`}
                value={form.convergence}
                onValueChange={value => onChange({ convergence: value as BPSAccountForm['convergence'] })}
                options={BPS_CONVERGENCE_MODES.map(value => ({ value, label: t(`accounts.bps.convergenceModes.${value}`) }))}
                disabled={disabled}
                aria-label={t('accounts.bps.convergence')}
              />
            </div>
            <div className="space-y-1.5">
              <label htmlFor={`${id}-native`} className="text-xs font-medium">{t('accounts.bps.native')}</label>
              <Select
                id={`${id}-native`}
                value={form.native}
                onValueChange={value => onChange({ native: value as BPSTriState })}
                options={triStateOptions(t('accounts.bps.inheritNative'))}
                disabled={disabled}
                aria-label={t('accounts.bps.native')}
              />
            </div>
            <div className="flex items-center justify-between gap-3 rounded-lg border border-border/60 px-3 py-2">
              <label htmlFor={`${id}-trim`} className="text-xs font-medium">{t('accounts.bps.imageTrim')}</label>
              <Switch
                id={`${id}-trim`}
                checked={form.imageTrim}
                onCheckedChange={imageTrim => onChange({ imageTrim })}
                disabled={disabled}
                aria-label={t('accounts.bps.imageTrim')}
              />
            </div>
          </div>
          <div className="grid gap-3 sm:grid-cols-2">
            <div className="space-y-1.5">
              <label htmlFor={`${id}-bps-models`} className="text-xs font-medium">{t('accounts.bps.bpsModels')}</label>
              <Input
                id={`${id}-bps-models`}
                value={form.bpsModels}
                onChange={event => onChange({ bpsModels: event.target.value })}
                placeholder={t('accounts.bps.modelsPlaceholder')}
                disabled={disabled}
              />
            </div>
            <div className="space-y-1.5">
              <label htmlFor={`${id}-native-models`} className="text-xs font-medium">{t('accounts.bps.nativeModels')}</label>
              <Input
                id={`${id}-native-models`}
                value={form.nativeModels}
                onChange={event => onChange({ nativeModels: event.target.value })}
                placeholder={t('accounts.bps.modelsPlaceholder')}
                disabled={disabled}
              />
            </div>
          </div>
          <p className="text-xs text-muted-foreground">{t('accounts.bps.routesHelp')}</p>
        </>
      )}
    </div>
  )
}
