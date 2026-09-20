import type { AccountProbeMode, AccountRow } from "../types";

export interface AccountProbePolicy {
  api_auto_recovery_enabled: boolean;
  probe_mode: AccountProbeMode;
  probe_interval_minutes: number;
}

export function accountProbePolicyFromAccount(
  account: Partial<AccountProbePolicy> = {},
): AccountProbePolicy {
  return {
    api_auto_recovery_enabled: account.api_auto_recovery_enabled ?? false,
    probe_mode: account.probe_mode ?? "auto",
    probe_interval_minutes: account.probe_interval_minutes ?? 0,
  };
}

export function accountProbePolicyChanged(
  draft: AccountProbePolicy,
  account: Partial<AccountProbePolicy>,
): boolean {
  const saved = accountProbePolicyFromAccount(account);
  return draft.probe_mode !== saved.probe_mode ||
    draft.probe_interval_minutes !== saved.probe_interval_minutes ||
    draft.api_auto_recovery_enabled !== saved.api_auto_recovery_enabled;
}

export function isAPIKeyProbeAccount(account: Pick<
  AccountRow,
  "openai_responses_api" | "grok_auth_kind" | "claude_auth_kind" | "antigravity_auth_kind"
>): boolean {
  return account.openai_responses_api === true ||
    account.grok_auth_kind === "api_key" ||
    account.claude_auth_kind === "api_key" ||
    account.antigravity_auth_kind === "api_key";
}
