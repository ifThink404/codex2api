import type { BatchUpdateAccountsRequest, CodexFingerprintMode } from "../types";

export interface BuildBatchMetadataUpdateOptions {
  ids: number[];
  updateTags: boolean;
  tags: string[];
  updateGroups: boolean;
  groupIds: number[];
  updateScoreBias: boolean;
  scoreBias: number | null;
  updateBaseConcurrency: boolean;
  baseConcurrency: number | null;
  updateSkipWarmTier?: boolean;
  skipWarmTier?: boolean;
  updateSchedulerPriority: boolean;
  schedulerPriority: number | null;
  updateCodexNativeEnabled?: boolean;
  codexNativeEnabled?: boolean;
  updateCodexRouteModels?: boolean;
  codexNativeModels?: string[];
  codexBPSModels?: string[];
  updateCodexBPSEnabled?: boolean;
  updateCodexBPSImageTrimEnabled?: boolean;
  updateCodexNativeCompactionOnly?: boolean;
  codexBPSEnabled?: boolean;
  codexBPSImageTrimEnabled?: boolean;
  codexNativeCompactionOnly?: boolean;
  updateCodexFingerprintMode?: boolean;
  codexFingerprintMode?: CodexFingerprintMode;
  updateSessionCapacity?: boolean;
  sessionCapacityEnabled?: boolean;
  sessionCapacityMax?: number;
  sessionCapacityReserved?: number;
  sessionCapacityIdleTTLSeconds?: number;
}

export function buildBatchMetadataUpdate({
  ids,
  updateTags,
  tags,
  updateGroups,
  groupIds,
  updateScoreBias,
  scoreBias,
  updateBaseConcurrency,
  baseConcurrency,
  updateSkipWarmTier,
  skipWarmTier,
  updateSchedulerPriority,
  schedulerPriority,
  updateCodexNativeEnabled,
  codexNativeEnabled,
  updateCodexRouteModels,
  codexNativeModels,
  codexBPSModels,
  updateCodexBPSEnabled,
  updateCodexBPSImageTrimEnabled,
  updateCodexNativeCompactionOnly,
  codexBPSEnabled,
  codexBPSImageTrimEnabled,
  codexNativeCompactionOnly,
  updateCodexFingerprintMode,
  codexFingerprintMode,
  updateSessionCapacity,
  sessionCapacityEnabled,
  sessionCapacityMax,
  sessionCapacityReserved,
  sessionCapacityIdleTTLSeconds,
}: BuildBatchMetadataUpdateOptions): BatchUpdateAccountsRequest {
  const payload: BatchUpdateAccountsRequest = { ids: [...ids] };
  if (updateTags) payload.tags = [...tags];
  if (updateGroups) payload.group_ids = [...groupIds];
  if (updateScoreBias) payload.score_bias_override = scoreBias;
  if (updateBaseConcurrency)
    payload.base_concurrency_override = baseConcurrency;
  if (updateSkipWarmTier) payload.skip_warm_tier = skipWarmTier ?? false;
  if (updateSchedulerPriority) payload.scheduler_priority = schedulerPriority;
  if (updateCodexNativeEnabled) payload.codex_native_enabled = codexNativeEnabled ?? true;
  if (updateCodexRouteModels) {
    payload.codex_native_models = [...(codexNativeModels ?? [])];
    payload.codex_bps_models = [...(codexBPSModels ?? [])];
  }
  if (updateCodexBPSEnabled) payload.codex_bps_enabled = codexBPSEnabled ?? false;
  if (updateCodexBPSImageTrimEnabled) payload.codex_bps_image_trim_enabled = codexBPSImageTrimEnabled ?? false;
  if (updateCodexNativeCompactionOnly) payload.codex_native_compaction_only = codexNativeCompactionOnly ?? false;
  if (updateCodexFingerprintMode)
    payload.codex_fingerprint_mode = codexFingerprintMode ?? "off";
  if (updateSessionCapacity) {
    payload.session_capacity_enabled = sessionCapacityEnabled ?? false;
    payload.session_capacity_max = sessionCapacityMax ?? 5;
    if (sessionCapacityReserved !== undefined) payload.session_capacity_reserved = sessionCapacityReserved;
    payload.session_capacity_idle_ttl_seconds =
      sessionCapacityIdleTTLSeconds ?? 3600;
  }
  return payload;
}
