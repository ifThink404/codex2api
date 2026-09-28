import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const testConnectionModal = readFileSync(new URL('../components/TestConnectionModal.tsx', import.meta.url), 'utf8')
const accountsPage = readFileSync(new URL('../pages/Accounts.tsx', import.meta.url), 'utf8')

test('Codex connection test keeps account-declared models alongside the catalog and on fallback', () => {
  assert.match(testConnectionModal, /\[\.\.\.accountModels, \.\.\.upstreamModels\]/)
  assert.match(testConnectionModal, /uniqueTestModels\(\s*\(account\.models \?\? \[\]\)\.filter\(isConnectionTestModel\),\s*DEFAULT_TEST_MODEL,/)
})

test('upstream model sync reports auto-added whitelist entries', () => {
  assert.match(accountsPage, /result\.whitelist_added\?\.length/)
  assert.match(accountsPage, /accounts\.supportedModelsSyncDoneWithAutoAdded/)
  assert.match(accountsPage, /accounts\.supportedModelsSyncHint/)
})
