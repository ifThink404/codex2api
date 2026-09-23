import type { AccountModelObservation } from "../types";

type Source = {
  models?: string[];
  model_observations?: AccountModelObservation[];
};
export type ModelAvailabilityState =
  AccountModelObservation["outcome"] | "unknown" | "configured" | "stale";
export function accountModelAvailability(
  account: Source,
  model: string,
  now = Date.now() / 1000,
): {
  state: ModelAvailabilityState;
  blocked: boolean;
  observation?: AccountModelObservation;
} {
  const key = model.trim().toLowerCase();
  const configured = (account.models ?? []).some(
    (m) => m.toLowerCase() === key,
  );
  const blocked = !!account.models?.length && !configured;
  const observation = (account.model_observations ?? [])
    .filter(
      (o) =>
        o.model.toLowerCase() === key &&
        o.observed_at <= now + 60,
    )
    .sort(
      (a, b) =>
        b.observed_at - a.observed_at ||
        Number(b.source === "probe") - Number(a.source === "probe"),
    )[0];
  const state = observation
    ? now - observation.observed_at > 86400
      ? "stale"
      : observation.outcome
    : configured
      ? "configured"
      : "unknown";
  return { state, blocked, observation };
}

export function isCodexModelAccount(account: {
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
