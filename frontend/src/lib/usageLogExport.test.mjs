import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

import { confirmedUsageLogDownload } from './usageLogExport.ts'

const usagePage = readFileSync(new URL('../pages/Usage.tsx', import.meta.url), 'utf8')
const apiSource = readFileSync(new URL('../api.ts', import.meta.url), 'utf8')
const locales = ['zh', 'en', 'zh-TW'].map((name) => JSON.parse(readFileSync(new URL(`../locales/${name}.json`, import.meta.url), 'utf8')))

test('usage log download only requests the file after confirmation', async () => {
  let downloads = 0
  let saved = null
  assert.equal(await confirmedUsageLogDownload(async () => false, async () => { downloads++; return 'blob' }, (blob) => { saved = blob }), false)
  assert.equal(downloads, 0)
  assert.equal(saved, null)
  assert.equal(await confirmedUsageLogDownload(async () => true, async () => { downloads++; return 'blob' }, (blob) => { saved = blob }), true)
  assert.equal(downloads, 1)
  assert.equal(saved, 'blob')
})

test('usage page wires confirmed filtered/all downloads through the shared API client', () => {
  assert.match(apiSource, /downloadUsageLogs: \(scope: 'filtered' \| 'all'/)
  assert.match(apiSource, /search\.set\('confirmed', 'true'\)/)
  assert.match(apiSource, /requestBlob\(`\/usage\/logs\/export\?/)
  assert.match(usagePage, /api\.downloadUsageLogs\(scope, params, controller\.signal\)/)
  assert.match(usagePage, /downloadLogs\('filtered'\)/)
  assert.match(usagePage, /downloadLogs\('all'\)/)
  for (const locale of locales) {
    for (const key of ['exportFiltered', 'exportAll', 'exportFilteredTitle', 'exportAllTitle', 'exportFilteredDesc', 'exportAllDesc', 'exportPrivacy', 'exportConfirm', 'exportCancel', 'exportSuccess', 'exportFailed']) {
      assert.equal(typeof locale.usage[key], 'string', key)
    }
  }
})
