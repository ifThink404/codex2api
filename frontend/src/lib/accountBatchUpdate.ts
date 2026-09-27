import type { BatchUpdateAccountsRequest, CodexFingerprintMode, CodexBPSProfile } from "../types";

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
  updateCodexSettings?: boolean;
  updateCodexNativeEnabled?: boolean;
  codexNativeEnabled?: boolean;
  updateUsageLimitBypass?: boolean;
  usageLimitBypassEnabled?: boolean;
  usageLimitBypassModels?: string[];
  updateCodexBPSEnabled?: boolean;
  updateCodexBPSProfile?: boolean;
  codexBPSProfile?: CodexBPSProfile;
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
  updateCodexSettings,
  updateCodexNativeEnabled,
  codexNativeEnabled,
  updateUsageLimitBypass,
  usageLimitBypassEnabled,
  usageLimitBypassModels,
  updateCodexBPSEnabled,
  updateCodexBPSProfile,
  codexBPSProfile,
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
  if (updateCodexSettings ?? updateCodexNativeEnabled) payload.codex_native_enabled = codexNativeEnabled ?? true;
  if (updateCodexSettings ?? updateUsageLimitBypass) {
    payload.codex_usage_limit_bypass_enabled = usageLimitBypassEnabled ?? false;
    payload.codex_usage_limit_bypass_models = [...(usageLimitBypassModels ?? [])];
  }
  if (updateCodexSettings ?? updateCodexBPSEnabled) payload.codex_bps_enabled = codexBPSEnabled ?? false;
  if (updateCodexSettings ?? updateCodexBPSProfile) payload.codex_bps_profile = codexBPSProfile ?? "word";
  if (updateCodexSettings ?? updateCodexBPSImageTrimEnabled) payload.codex_bps_image_trim_enabled = codexBPSImageTrimEnabled ?? false;
  if (updateCodexSettings ?? updateCodexNativeCompactionOnly) payload.codex_native_compaction_only = codexNativeCompactionOnly ?? false;
  if (updateCodexSettings ?? updateCodexFingerprintMode)
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
