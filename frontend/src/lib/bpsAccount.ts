// Optional BPS configuration owns its eligibility rule independently of models.
export function isBPSAccount(account: {
  openai_responses_api?: boolean;
  grok_api?: boolean;
  claude_api?: boolean;
  antigravity_api?: boolean;
  agent_identity?: boolean;
}) {
  return (
    !account.openai_responses_api &&
    !account.grok_api &&
    !account.claude_api &&
    !account.antigravity_api &&
    !account.agent_identity
  );
}
