import type { BatchUpdateAccountsRequest, CodexFingerprintMode } from "../types";
import type { AccountProbePolicy } from "./accountProbePolicy";

export interface BuildBatchMetadataUpdateOptions {
  updateProbePolicy?: boolean;
  probePolicy?: AccountProbePolicy;
  ids: number[];
  updateTags: boolean;
  tags: string[];
  updateGroups: boolean;
  groupIds: number[];
  updateScoreBias: boolean;
  scoreBias: number | null;
  updateBaseConcurrency: boolean;
  baseConcurrency: number | null;
  updateSchedulerPriority: boolean;
  schedulerPriority: number | null;
  updateCodexFingerprintMode?: boolean;
  codexFingerprintMode?: CodexFingerprintMode;
  updateTimezone?: boolean;
  timezone?: string;
}

export function buildBatchMetadataUpdate({
  updateProbePolicy,
  probePolicy,
  ids,
  updateTags,
  tags,
  updateGroups,
  groupIds,
  updateScoreBias,
  scoreBias,
  updateBaseConcurrency,
  baseConcurrency,
  updateSchedulerPriority,
  schedulerPriority,
  updateCodexFingerprintMode,
  codexFingerprintMode,
  updateTimezone,
  timezone,
}: BuildBatchMetadataUpdateOptions): BatchUpdateAccountsRequest {
  const payload: BatchUpdateAccountsRequest = { ids: [...ids] };
  if (updateProbePolicy && probePolicy) {
    payload.probe_mode = probePolicy.probe_mode;
    payload.probe_interval_minutes = probePolicy.probe_interval_minutes;
    payload.api_auto_recovery_enabled = probePolicy.api_auto_recovery_enabled;
  }
  if (updateTags) payload.tags = [...tags];
  if (updateGroups) payload.group_ids = [...groupIds];
  if (updateScoreBias) payload.score_bias_override = scoreBias;
  if (updateBaseConcurrency)
    payload.base_concurrency_override = baseConcurrency;
  if (updateSchedulerPriority) payload.scheduler_priority = schedulerPriority;
  if (updateCodexFingerprintMode)
    payload.codex_fingerprint_mode = codexFingerprintMode ?? "off";
  if (updateTimezone) payload.timezone = (timezone ?? "").trim();
  return payload;
}
