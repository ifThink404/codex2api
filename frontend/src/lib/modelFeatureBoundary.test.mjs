import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

test('model availability modules do not import optional BPS features or expose them in UI', () => {
  for (const relative of ['./accountModelAvailability.ts', '../components/AccountModelAvailability.tsx']) {
    const source = readFileSync(new URL(relative, import.meta.url), 'utf8')
    assert.doesNotMatch(source, /bps/i, relative)
  }
})
