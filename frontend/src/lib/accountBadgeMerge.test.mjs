import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

test('ordinary Codex accounts retain the model badge container after template removal', () => {
  const source = readFileSync(new URL('../pages/Accounts.tsx', import.meta.url), 'utf8')
  const tableRow = source.split('const AccountTableRow = memo(')[1]?.split('const AccountCardItem = memo(')[0] ?? ''
  assert.match(tableRow, /\{\(isCodexOfficialAccount\(account\) \|\|/)
  assert.match(tableRow, /<AccountModelAvailabilityBadge account=\{account\}/)
  assert.doesNotMatch(tableRow, /CodexTurnStateBadge|isCodexTurnStateAccount/)
})
