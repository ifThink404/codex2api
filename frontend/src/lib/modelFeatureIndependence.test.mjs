import assert from 'node:assert/strict'
import test from 'node:test'
import { accountModelAvailability } from './accountModelAvailability.ts'

test('model status does not depend on optional request transport settings', () => {
  const observation = {model:'gpt-6-sol',source:'probe',outcome:'available',observed_at:100}
  for (const enabled of [undefined, false, true]) {
    const account = {codex_bps_enabled:enabled,models:[],model_observations:[observation]}
    assert.equal(accountModelAvailability(account,'gpt-6-sol',101).state,'available')
  }
})
