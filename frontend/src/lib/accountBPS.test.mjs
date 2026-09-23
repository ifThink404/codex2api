import assert from 'node:assert/strict'
import test from 'node:test'
import { formStateFromAccount, buildQuickConfigSavePayload } from './accountQuickConfig.ts'

test('BPS round trips enabled and disabled in account quick configuration', () => {
 for (const enabled of [true, false]) {
  const form = formStateFromAccount({id:1,codex_bps_enabled:enabled})
  assert.equal(form.bpsEnabled,enabled)
  assert.equal(buildQuickConfigSavePayload(form,true).payload.codex_bps_enabled,enabled)
 }
})
