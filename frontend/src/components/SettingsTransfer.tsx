import { useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Download, Upload } from 'lucide-react'
import { api } from '../api'
import { useToast } from '../hooks/useToast'
import { getErrorMessage } from '../utils/error'
import { parseSettingsBackup, SETTINGS_BACKUP_MAX_BYTES, SettingsImportError, type SettingsBackup } from '../lib/settingsTransfer'
import { Button } from './ui/button'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from './ui/dialog'

async function readSettingsBackup(): Promise<SettingsBackup> {
  const [main, claude, antigravity, tests, invite, visible] = await Promise.all([
    api.exportSettings(), api.getClaudeConfig(), api.getAntigravitySettings(),
    api.getChannelTestSettings(), api.getInviteGuideSettings(), api.getVisibleChannels(),
  ])
  const { synced_cli_version: _synced, builtin_cli_version: _builtin, effective_cli_version: _effective, ...claudeWritable } = claude
  claudeWritable.allowed_beta_headers ??= []
  return { ...main, sections: {
    claude: claudeWritable,
    antigravity: { model_redirects: antigravity.model_redirects, redirect_overrides_effort: antigravity.redirect_overrides_effort },
    channel_tests: { claude: tests.claude, antigravity: tests.antigravity },
    invite_guide: { enabled: invite.enabled }, visible_channels: { channels: visible.channels },
  } }
}

export default function SettingsTransfer({ disabled }: { disabled: boolean }) {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const input = useRef<HTMLInputElement>(null)
  const [busy, setBusy] = useState(false)
  const [pending, setPending] = useState<{ name: string; backup: SettingsBackup } | null>(null)
  const [error, setError] = useState('')
  const [applied, setApplied] = useState<string[]>([])
  const [complete, setComplete] = useState(false)

  const exportSettings = async () => {
    setBusy(true)
    try {
      const backup = await readSettingsBackup()
      const url = URL.createObjectURL(new Blob([JSON.stringify(backup, null, 2)], { type: 'application/json' }))
      const link = document.createElement('a')
      link.href = url
      link.download = `codex2api-settings-${new Date().toISOString().replace(/[:.]/g, '-')}.json`
      document.body.appendChild(link)
      link.click()
      link.remove()
      window.setTimeout(() => URL.revokeObjectURL(url), 1000)
      showToast(t('settings.transfer.exported'), 'success')
    } catch (cause) { showToast(getErrorMessage(cause), 'error') }
    finally { setBusy(false) }
  }

  const readFile = async (file: File) => {
    if (file.size > SETTINGS_BACKUP_MAX_BYTES) { showToast(t('settings.transfer.tooLarge'), 'error'); return }
    setBusy(true)
    setError('')
    setApplied([])
    setComplete(false)
    try {
      const [text, target] = await Promise.all([file.text(), readSettingsBackup()])
      setPending({ name: file.name, backup: parseSettingsBackup(text, target) })
    } catch (cause) {
      showToast(cause instanceof SettingsImportError ? t(`settings.transfer.invalid_${cause.reason}`, { field: cause.field }) : getErrorMessage(cause), 'error')
    } finally { setBusy(false) }
  }

  const importSettings = async () => {
    if (!pending) return
    setBusy(true)
    setError('')
    const done: string[] = []
    let current = 'settings'
    try {
      const backup = pending.backup
      // Each section retains the validation/persistence semantics of its normal
      // settings endpoint. Report precisely which sections succeeded on failure.
      const steps: Array<[string, () => Promise<unknown>]> = []
      if (Object.keys(backup.settings).length) steps.push(['settings', () => api.updateSettings(backup.settings)])
      const sections = backup.sections
      if (sections?.claude) steps.push(['claude', async () => api.updateClaudeConfig({ ...await api.getClaudeConfig(), ...sections.claude! })])
      if (sections?.antigravity) steps.push(['antigravity', () => api.updateAntigravitySettings(sections.antigravity!)])
      if (sections?.channel_tests) steps.push(['channel_tests', () => api.updateChannelTestSettings(sections.channel_tests!)])
      if (sections?.invite_guide) steps.push(['invite_guide', () => api.updateInviteGuideSettings(sections.invite_guide!.enabled)])
      if (sections?.visible_channels) steps.push(['visible_channels', () => api.updateVisibleChannels(sections.visible_channels!.channels)])
      for (const [name, save] of steps) {
        current = name
        await save()
        done.push(name)
        setApplied([...done])
      }
      setComplete(true)
    } catch (cause) {
      setError(t('settings.transfer.failedSection', { section: t(`settings.transfer.sections.${current}`), error: getErrorMessage(cause) }))
    } finally { setBusy(false) }
  }

  return <>
    <input ref={input} type="file" accept=".json,application/json" className="hidden" aria-label={t('settings.transfer.import')}
      onChange={(event) => { const file = event.currentTarget.files?.[0]; event.currentTarget.value = ''; if (file) void readFile(file) }} />
    <Button variant="outline" size="sm" disabled={disabled || busy} onClick={() => void exportSettings()} title={t('settings.transfer.exportHint')}><Download className="size-4" />{t('settings.transfer.export')}</Button>
    <Button variant="outline" size="sm" disabled={disabled || busy} onClick={() => input.current?.click()} title={t('settings.transfer.importHint')}><Upload className="size-4" />{t('settings.transfer.import')}</Button>
    <Dialog open={pending !== null} onOpenChange={(open) => { if (!open && !busy) { if (applied.length) window.location.reload(); else setPending(null) } }}>
      <DialogContent>
        <DialogHeader><DialogTitle>{t(complete ? 'settings.transfer.imported' : 'settings.transfer.import')}</DialogTitle>
          <DialogDescription>{pending?.name}</DialogDescription></DialogHeader>
        <p className="text-sm text-muted-foreground">{t('settings.transfer.scope')}</p>
        {pending && <p className="text-sm">{t('settings.transfer.summary', { count: Object.keys(pending.backup.settings).length, sections: Object.keys(pending.backup.sections ?? {}).map((name) => t(`settings.transfer.sections.${name}`)).join('、') || '—' })}</p>}
        {applied.length > 0 && <p className="text-sm">{t('settings.transfer.applied', { sections: applied.map((name) => t(`settings.transfer.sections.${name}`)).join('、') })}</p>}
        {error && <p role="alert" className="text-sm text-destructive break-words">{error}</p>}
        <DialogFooter>
          {complete || applied.length > 0 ? <Button disabled={busy} onClick={() => window.location.reload()}>{t('settings.transfer.refresh')}</Button> : <Button variant="outline" disabled={busy} onClick={() => setPending(null)}>{t('common.cancel')}</Button>}
          {!complete && !applied.length && <Button disabled={busy} onClick={() => void importSettings()}>{t(busy ? 'settings.transfer.importing' : 'settings.transfer.confirm')}</Button>}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  </>
}
