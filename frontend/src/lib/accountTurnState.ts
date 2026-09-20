import type { AccountRow } from '../types'

// The turn-state observer runs only on the official Codex request path.
// Keep legitimate unrecorded samples there; other providers have no evidence.
export function officialAccountLatestTurnState(account: Pick<AccountRow,
  'openai_responses_api' | 'grok_api' | 'claude_api' | 'antigravity_api' | 'latest_turn_state'
>) {
  if (account.openai_responses_api || account.grok_api || account.claude_api || account.antigravity_api) {
    return undefined
  }
  return account.latest_turn_state
}
