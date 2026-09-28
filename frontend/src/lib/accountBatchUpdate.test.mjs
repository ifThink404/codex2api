import assert from "node:assert/strict";
import test from "node:test";

import { buildBatchMetadataUpdate } from "./accountBatchUpdate.ts";

test("batch Codex defaults stay off unless explicitly enabled", () => {
  const base = { ids: [1, 2], updateTags: false, tags: [], updateGroups: false, groupIds: [], updateScoreBias: false, scoreBias: null, updateBaseConcurrency: false, baseConcurrency: null, updateSchedulerPriority: false, schedulerPriority: null };
  for (const selection of [{ updateCodexSettings: true }, { updateCodexNativeEnabled: true }]) {
    const options = { ...base, ...selection };
    assert.equal(buildBatchMetadataUpdate(options).codex_native_enabled, false);
    assert.equal(buildBatchMetadataUpdate({ ...options, codexNativeEnabled: false }).codex_native_enabled, false);
    assert.equal(buildBatchMetadataUpdate({ ...options, codexNativeEnabled: true }).codex_native_enabled, true);
  }
  assert.deepEqual(buildBatchMetadataUpdate({ ...base, updateCodexSettings: false, updateCodexNativeEnabled: true, codexNativeEnabled: true }), { ids: [1, 2] });
});

test("one Codex settings switch applies all values and omits them when off", () => {
  const base = { ids: [1, 2], updateTags: true, tags: ["keep"], updateGroups: false, groupIds: [], updateScoreBias: false, scoreBias: null, updateBaseConcurrency: false, baseConcurrency: null, updateSchedulerPriority: false, schedulerPriority: null };
  const settings = { codexNativeEnabled: true, codexBPSEnabled: true, usageLimitBypassEnabled: true, usageLimitBypassModels: ["gpt-5.6-sol"], codexBPSProfile: "word", codexBPSImageTrimEnabled: true, codexNativeCompactionOnly: false, codexFingerprintMode: "off" };
  assert.deepEqual(buildBatchMetadataUpdate({ ...base, ...settings, updateCodexSettings: false }), { ids: [1, 2], tags: ["keep"] });
  assert.deepEqual(buildBatchMetadataUpdate({ ...base, ...settings, updateCodexSettings: true }), {
    ids: [1, 2], tags: ["keep"], codex_native_enabled: true, codex_bps_enabled: true,
    codex_usage_limit_bypass_enabled: true, codex_usage_limit_bypass_models: ["gpt-5.6-sol"],
    codex_bps_profile: "word", codex_bps_image_trim_enabled: true,
    codex_native_compaction_only: false, codex_fingerprint_mode: "off",
  });
  const disabled = buildBatchMetadataUpdate({ ...base, ...settings, updateCodexSettings: true, codexBPSEnabled: false, codexBPSImageTrimEnabled: false, usageLimitBypassEnabled: false, usageLimitBypassModels: [] });
  assert.equal(disabled.codex_bps_enabled, false);
  assert.equal(disabled.codex_bps_image_trim_enabled, false);
  assert.equal(disabled.codex_usage_limit_bypass_enabled, false);
  assert.deepEqual(disabled.codex_usage_limit_bypass_models, []);
  assert.deepEqual(buildBatchMetadataUpdate({ ...base, ...settings, updateCodexSettings: false, updateCodexBPSEnabled: true }), { ids: [1, 2], tags: ["keep"] });
});

test("BPS profile batch edit preserves mixed values unless explicitly selected", () => {
  const base = { ids: [1, 2], updateTags: false, tags: [], updateGroups: false, groupIds: [], updateScoreBias: false, scoreBias: null, updateBaseConcurrency: false, baseConcurrency: null, updateSchedulerPriority: false, schedulerPriority: null };
  assert.deepEqual(buildBatchMetadataUpdate({ ...base, codexBPSProfile: "excel" }), { ids: [1, 2] });
  assert.deepEqual(buildBatchMetadataUpdate({ ...base, updateCodexBPSProfile: true }), { ids: [1, 2], codex_bps_profile: "word" });
  for (const profile of ["word", "excel", "sheets", "powerpoint"]) {
    assert.deepEqual(buildBatchMetadataUpdate({ ...base, updateCodexBPSProfile: true, codexBPSProfile: profile }), { ids: [1, 2], codex_bps_profile: profile });
  }
});

test("native compaction restriction is explicit and can be disabled independently", () => {
  const base = { ids: [1, 2], updateTags: false, tags: [], updateGroups: false, groupIds: [], updateScoreBias: false, scoreBias: null, updateBaseConcurrency: false, baseConcurrency: null, updateSchedulerPriority: false, schedulerPriority: null };
  assert.deepEqual(buildBatchMetadataUpdate({ ...base, codexNativeCompactionOnly: true }), { ids: [1, 2] });
  for (const enabled of [true, false]) {
    assert.deepEqual(buildBatchMetadataUpdate({ ...base, updateCodexNativeCompactionOnly: true, codexNativeCompactionOnly: enabled }), { ids: [1, 2], codex_native_compaction_only: enabled });
  }
});

test("image history trimming is explicit and can be disabled without changing BPS routing", () => {
  const base = { ids: [1, 2], updateTags: false, tags: [], updateGroups: false, groupIds: [], updateScoreBias: false, scoreBias: null, updateBaseConcurrency: false, baseConcurrency: null, updateSchedulerPriority: false, schedulerPriority: null };
  assert.deepEqual(buildBatchMetadataUpdate({ ...base, codexBPSImageTrimEnabled: true }), { ids: [1, 2] });
  for (const enabled of [true, false]) {
    assert.deepEqual(buildBatchMetadataUpdate({ ...base, updateCodexBPSImageTrimEnabled: true, codexBPSImageTrimEnabled: enabled }), { ids: [1, 2], codex_bps_image_trim_enabled: enabled });
  }
});

test("buildBatchMetadataUpdate includes enabled scheduler fields", () => {
  const payload = buildBatchMetadataUpdate({
    ids: [3, 7],
    updateTags: false,
    tags: ["ignored"],
    updateGroups: false,
    groupIds: [99],
    updateScoreBias: true,
    scoreBias: 25,
    updateBaseConcurrency: true,
    baseConcurrency: 4,
    updateSkipWarmTier: true,
    skipWarmTier: true,
    updateSchedulerPriority: true,
    schedulerPriority: 10,
  });

  assert.deepEqual(payload, {
    ids: [3, 7],
    score_bias_override: 25,
    base_concurrency_override: 4,
    skip_warm_tier: true,
    scheduler_priority: 10,
  });
});

test("buildBatchMetadataUpdate omits the fingerprint mode unless explicitly enabled", () => {
  const untouched = buildBatchMetadataUpdate({
    ids: [1],
    updateTags: false,
    tags: [],
    updateGroups: false,
    groupIds: [],
    updateScoreBias: false,
    scoreBias: null,
    updateBaseConcurrency: false,
    baseConcurrency: null,
    updateSchedulerPriority: false,
    schedulerPriority: null,
    updateCodexFingerprintMode: false,
    codexFingerprintMode: "session",
  });

  assert.deepEqual(untouched, { ids: [1] });

  const applied = buildBatchMetadataUpdate({
    ids: [1],
    updateTags: false,
    tags: [],
    updateGroups: false,
    groupIds: [],
    updateScoreBias: false,
    scoreBias: null,
    updateBaseConcurrency: false,
    baseConcurrency: null,
    updateSchedulerPriority: false,
    schedulerPriority: null,
    updateCodexFingerprintMode: true,
    codexFingerprintMode: "session",
  });

  assert.deepEqual(applied, { ids: [1], codex_fingerprint_mode: "session" });
});

test("buildBatchMetadataUpdate sends null only for enabled reset fields", () => {
  const payload = buildBatchMetadataUpdate({
    ids: [5],
    updateTags: false,
    tags: [],
    updateGroups: false,
    groupIds: [],
    updateScoreBias: true,
    scoreBias: null,
    updateBaseConcurrency: false,
    baseConcurrency: null,
    updateSchedulerPriority: true,
    schedulerPriority: null,
  });

  assert.deepEqual(payload, {
    ids: [5],
    score_bias_override: null,
    scheduler_priority: null,
  });
});

test("buildBatchMetadataUpdate applies session capacity only when selected", () => {
  const untouched = buildBatchMetadataUpdate({
    ids: [2, 4],
    updateTags: false,
    tags: [],
    updateGroups: false,
    groupIds: [],
    updateScoreBias: false,
    scoreBias: null,
    updateBaseConcurrency: false,
    baseConcurrency: null,
    updateSchedulerPriority: false,
    schedulerPriority: null,
    updateSessionCapacity: false,
    sessionCapacityEnabled: true,
    sessionCapacityMax: 8,
    sessionCapacityIdleTTLSeconds: 7200,
  });
  assert.deepEqual(untouched, { ids: [2, 4] });

  const applied = buildBatchMetadataUpdate({
    ids: [2, 4],
    updateTags: false,
    tags: [],
    updateGroups: false,
    groupIds: [],
    updateScoreBias: false,
    scoreBias: null,
    updateBaseConcurrency: false,
    baseConcurrency: null,
    updateSchedulerPriority: false,
    schedulerPriority: null,
    updateSessionCapacity: true,
    sessionCapacityEnabled: true,
    sessionCapacityMax: 8,
    sessionCapacityIdleTTLSeconds: 7200,
  });
  assert.deepEqual(applied, {
    ids: [2, 4],
    session_capacity_enabled: true,
    session_capacity_max: 8,
    session_capacity_idle_ttl_seconds: 7200,
  });
});


test("BPS batch setting changes only when explicitly selected, including false", () => {
  const options = { ids: [1], updateTags: false, tags: [], updateGroups: false, groupIds: [], updateScoreBias: false, scoreBias: null, updateBaseConcurrency: false, baseConcurrency: null, updateSchedulerPriority: false, schedulerPriority: null };
  assert.deepEqual(buildBatchMetadataUpdate({...options, codexBPSEnabled: true}), {ids:[1]});
  assert.deepEqual(buildBatchMetadataUpdate({...options, updateCodexBPSEnabled:true, codexBPSEnabled:true}), {ids:[1],codex_bps_enabled:true});
  assert.deepEqual(buildBatchMetadataUpdate({...options, updateCodexBPSEnabled:true, codexBPSEnabled:false}), {ids:[1],codex_bps_enabled:false});
});


test("Codex switches and model quota exemptions require explicit batch selection", () => {
  const base = { ids: [1, 2], updateTags: false, tags: [], updateGroups: false, groupIds: [], updateScoreBias: false, scoreBias: null, updateBaseConcurrency: false, baseConcurrency: null, updateSchedulerPriority: false, schedulerPriority: null };
  assert.deepEqual(buildBatchMetadataUpdate({ ...base, codexNativeEnabled: false, codexBPSEnabled: true, codexNativeModels: ["gpt-5.6-*"], codexBPSModels: ["gpt-6-*"] }), { ids: [1, 2] });
  assert.deepEqual(buildBatchMetadataUpdate({ ...base, updateCodexNativeEnabled: true, codexNativeEnabled: false }), { ids: [1, 2], codex_native_enabled: false });
  assert.deepEqual(buildBatchMetadataUpdate({ ...base, updateCodexBPSEnabled: true, codexBPSEnabled: false }), { ids: [1, 2], codex_bps_enabled: false });
  assert.deepEqual(buildBatchMetadataUpdate({ ...base, updateUsageLimitBypass: true, usageLimitBypassEnabled: true, usageLimitBypassModels: ["gpt-5.6-sol"] }), { ids: [1, 2], codex_usage_limit_bypass_enabled: true, codex_usage_limit_bypass_models: ["gpt-5.6-sol"] });
  assert.deepEqual(buildBatchMetadataUpdate({ ...base, usageLimitBypassEnabled: true, usageLimitBypassModels: ["gpt-6-astra"] }), { ids: [1, 2] });
  assert.deepEqual(buildBatchMetadataUpdate({ ...base, updateUsageLimitBypass: true, usageLimitBypassEnabled: false, usageLimitBypassModels: [] }), { ids: [1, 2], codex_usage_limit_bypass_enabled: false, codex_usage_limit_bypass_models: [] });
});
