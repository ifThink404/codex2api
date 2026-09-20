import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";
import test from "node:test";
import { buildBatchMetadataUpdate } from "./accountBatchUpdate.ts";
import { formStateFromAccount, buildQuickConfigSavePayload } from "./accountQuickConfig.ts";

function source(path) {
  const url = new URL(path, import.meta.url);
  assert.ok(existsSync(url), `${path} must exist`);
  return readFileSync(url, "utf8");
}

test("account probe contract is shared by GET rows, scheduler PATCH and batch updates", () => {
  const types = source("../types.ts");
  assert.match(types, /export type AccountProbeMode = ['"]auto['"] \| ['"]off['"] \| ['"]on['"]/);
  for (const name of ["AccountRow", "UpdateAccountSchedulerRequest"]) {
    const body = types.split(`export interface ${name} {`)[1]?.split("\n}")[0] ?? "";
    assert.match(body, /probe_mode\?: AccountProbeMode/);
    assert.match(body, /probe_interval_minutes\?: number/);
    assert.match(body, /api_auto_recovery_enabled\?: boolean/);
  }
  assert.match(types, /BatchUpdateAccountsRequest extends UpdateAccountSchedulerRequest/);
  const api = source("../api.ts");
  assert.match(api, /updateAccountScheduler:[\s\S]*?\/accounts\/\$\{id\}\/scheduler`[^\n]*method: 'PATCH'/);
});

test("probe drafts inherit missing settings and round-trip every mode and interval boundary", async () => {
  source("./accountProbePolicy.ts");
  const { accountProbePolicyFromAccount, accountProbePolicyChanged } = await import("./accountProbePolicy.ts");
  const inherited = { probe_mode: "auto", probe_interval_minutes: 0, api_auto_recovery_enabled: false };
  assert.deepEqual(accountProbePolicyFromAccount(), inherited);
  for (const account of [{}, { openai_responses_api: true }, { grok_auth_kind: "api_key" }, { claude_auth_kind: "oauth" }, { antigravity_auth_kind: "api_key" }]) {
    assert.deepEqual(accountProbePolicyFromAccount(account), inherited);
    assert.equal(accountProbePolicyChanged(inherited, account), false);
  }
  for (const probe_mode of ["auto", "off", "on"]) {
    for (const probe_interval_minutes of [0, 1, 30, 1440]) {
      const policy = { probe_mode, probe_interval_minutes, api_auto_recovery_enabled: false };
      assert.deepEqual(accountProbePolicyFromAccount(policy), policy);
      assert.equal(accountProbePolicyChanged(policy, policy), false);
    }
  }
  assert.equal(accountProbePolicyChanged({ probe_mode: "off", probe_interval_minutes: 0 }, {}), true);
  assert.equal(accountProbePolicyChanged({ probe_mode: "auto", probe_interval_minutes: 1 }, {}), true);
  assert.equal(accountProbePolicyChanged(inherited, { probe_mode: "on", probe_interval_minutes: 60 }), true);
  const enabled = { ...inherited, api_auto_recovery_enabled: true };
  assert.deepEqual(accountProbePolicyFromAccount(enabled), enabled);
  assert.equal(accountProbePolicyChanged(enabled, {}), true);
  assert.equal(accountProbePolicyChanged(inherited, enabled), true);
});

test("API recovery controls require an explicit API-key account kind", async () => {
  const { isAPIKeyProbeAccount } = await import("./accountProbePolicy.ts");
  assert.equal(typeof isAPIKeyProbeAccount, "function");
  for (const account of [{}, { plan_type: "api" }, { grok_api: true }, { claude_auth_kind: "oauth" }, { antigravity_auth_kind: "oauth" }]) {
    assert.equal(isAPIKeyProbeAccount(account), false);
  }
  for (const account of [{ openai_responses_api: true }, { grok_auth_kind: "api_key" }, { claude_auth_kind: "api_key" }, { antigravity_auth_kind: "api_key" }]) {
    assert.equal(isAPIKeyProbeAccount(account), true);
  }
});

test("shared probe controls use draft inputs and explain all modes without sending requests", () => {
  const component = source("../components/AccountProbePolicyFields.tsx");
  assert.match(component, /<Select\b/);
  assert.match(component, /<DraftNumberInput[\s\S]*?min=\{0\}[\s\S]*?max=\{1440\}/);
  assert.match(component, /integer/);
  assert.match(component, /emptyValue=\{0\}/);
  assert.match(component, /apiAccount &&/);
  assert.match(component, /<Switch[\s\S]*?checked=\{value.api_auto_recovery_enabled\}/);
  assert.match(component, /api_auto_recovery_enabled: checked/);
  assert.match(component, /accounts.apiAutoRecoveryHint/);
  assert.match(component, /disabled=\{disabled \|\| value.probe_mode === "off"\}/);
  for (const mode of ["auto", "off", "on"]) assert.match(component, new RegExp(`value: "${mode}"`));
  assert.match(component, /onChange\(\{ \.\.\.value, probe_mode:/);
  assert.match(component, /onChange\(\{ \.\.\.value, probe_interval_minutes:/);
  assert.doesNotMatch(component, /\bapi\.|\bfetch\(|<select\b/);
  for (const key of ["probeMode", "probeModeAuto", "probeModeOff", "probeModeOn", "probeAutoHint", "probeOffHint", "probeOnHint", "probeInterval", "probeIntervalHint", "probeHealthHint"]) {
    assert.ok(component.includes(`accounts.${key}`), key);
  }
});

test("every account editor loads, renders and saves the probe draft", () => {
  const accounts = source("../pages/Accounts.tsx");
  assert.match(accounts, /setProbePolicy\(accountProbePolicyFromAccount\(account\)\)/);
  assert.match(accounts, /const handleSaveScheduler = [\s\S]*?const payload = \{\s*\.\.\.probePolicy,/);
  assert.match(accounts, /<AccountProbePolicyFields[\s\S]*?value=\{probePolicy\}[\s\S]*?onChange=\{setProbePolicy\}/);
  const schedulerBody = accounts.split('{/* 分组 1: 调度与并发加权 */}')[1]?.split('{/* 权重分 */}')[0] ?? "";
  assert.match(schedulerBody, /<AccountProbePolicyFields/);
  for (const [page, handler, providerMethod, id] of [
    ["GrokAccounts", "handleSaveEdit", "updateGrokAccount", "editAccount"],
    ["AntigravityAccounts", "handleEdit", "updateAntigravityAccount", "editingAccount"],
  ]) {
    const code = source(`../pages/${page}.tsx`);
    assert.match(code, /<AccountProbePolicyFields/);
    assert.match(code, /accountProbePolicyFromAccount\(account\)/);
    const save = code.split(`const ${handler} = async () => {`)[1]?.split("\n  }; ")[0] ?? "";
    assert.ok(save.indexOf(`api.${providerMethod}`) < save.indexOf("api.updateAccountScheduler"));
    assert.match(save, new RegExp(`accountProbePolicyChanged\\([^,]+, ${id}\\)`));
    assert.match(save, new RegExp(`await api\\.updateAccountScheduler\\(${id}\\.id,`));
    assert.match(save, /accounts.probeSaveFailedAfterAccountSave/);
  }
  const claude = source("../pages/ClaudeAccounts.tsx");
  const editor = claude.split("function EditAccountModal(")[1]?.split("function ClaudeModelsModal(")[0] ?? "";
  assert.match(editor, /accountProbePolicyFromAccount\(account\)/);
  assert.match(editor, /<AccountProbePolicyFields/);
  assert.match(editor, /api.updateAccountScheduler\(account.id, \{\s*\.\.\.probePolicy,/);
  assert.match(editor, /\}, \[account.id, probePolicy,/);
  assert.match(claude, /\.filter\(\(acc\) => acc.probe_mode !== "off" && acc.claude_api/);
});

test("quick configuration preserves probe policy through its existing save", () => {
  for (const probe_mode of ["auto", "off", "on"]) {
    const form = formStateFromAccount({ id: 42, probe_mode, probe_interval_minutes: 1440 });
    assert.deepEqual(form.probePolicy, { probe_mode, probe_interval_minutes: 1440, api_auto_recovery_enabled: false });
    const result = buildQuickConfigSavePayload(form, true);
    assert.equal(result.ok, true);
    assert.equal(result.payload.probe_mode, probe_mode);
    assert.equal(result.payload.probe_interval_minutes, 1440);
    assert.equal(result.payload.api_auto_recovery_enabled, false);
  }
  const sheet = source("../components/AccountQuickConfigSheet.tsx");
  assert.match(sheet, /<AccountProbePolicyFields[\s\S]*?value=\{form.probePolicy\}[\s\S]*?patchForm\(\{ probePolicy \}\)/);
});

test("batch probe changes are opt-in and preserve explicit auto and inherited zero", () => {
  const base = {
    ids: [2, 5], updateTags: false, tags: [], updateGroups: false, groupIds: [],
    updateScoreBias: false, scoreBias: null, updateBaseConcurrency: false,
    baseConcurrency: null, updateSchedulerPriority: false, schedulerPriority: null,
  };
  const probePolicy = { probe_mode: "on", probe_interval_minutes: 45, api_auto_recovery_enabled: true };
  assert.deepEqual(buildBatchMetadataUpdate({ ...base, updateProbePolicy: false, probePolicy }), { ids: [2, 5] });
  assert.deepEqual(buildBatchMetadataUpdate({ ...base, updateProbePolicy: true, probePolicy }), { ids: [2, 5], probe_mode: "on", probe_interval_minutes: 45, api_auto_recovery_enabled: true });
  assert.deepEqual(buildBatchMetadataUpdate({ ...base, updateProbePolicy: true, probePolicy: { probe_mode: "auto", probe_interval_minutes: 0, api_auto_recovery_enabled: false } }), { ids: [2, 5], probe_mode: "auto", probe_interval_minutes: 0, api_auto_recovery_enabled: false });
  const page = source("../pages/Accounts.tsx");
  assert.match(page, /const batchMetaHasUpdates =[\s\S]*?batchUpdateProbePolicy/);
  assert.match(page, /updateProbePolicy: batchUpdateProbePolicy/);
  assert.match(page, /probePolicy: batchProbePolicy/);
  assert.match(page, /<Switch\s+checked=\{batchUpdateProbePolicy\}/);
  assert.match(page, /<AccountProbePolicyFields\s+value=\{batchProbePolicy\}/);
  for (const opener of ["openBatchMetaEditor", "openBatchGroupEditor"]) {
    const body = page.split(`const ${opener} = () => {`)[1]?.split("\n  };")[0] ?? "";
    assert.match(body, /setBatchUpdateProbePolicy\(false\)/);
  }
});

test("probe labels and operational semantics are translated in all supported locales", () => {
  const keys = ["probePolicyTitle", "probeMode", "probeModeAuto", "probeModeOff", "probeModeOn", "probeAutoHint", "probeOffHint", "probeOnHint", "probeInterval", "probeIntervalHint", "probeHealthHint", "probeBatchApply", "probeSaveFailedAfterAccountSave", "apiAutoRecovery", "apiAutoRecoveryHint"];
  for (const locale of ["zh", "en", "zh-TW"]) {
    const messages = JSON.parse(source(`../locales/${locale}.json`)).accounts;
    for (const key of keys) assert.ok(typeof messages[key] === "string" && messages[key].trim(), `${locale}.accounts.${key}`);
  }
  const en = JSON.parse(source("../locales/en.json")).accounts;
  assert.match(en.probeAutoHint, /API.key[\s\S]*OAuth/i);
  assert.match(en.probeOffHint, /token renewal[\s\S]*manual/i);
  assert.match(en.probeHealthHint, /business requests[\s\S]*health[\s\S]*cooldown[\s\S]*unavailable/i);
  assert.match(en.apiAutoRecoveryHint, /401\/403[\s\S]*business traffic[\s\S]*off[\s\S]*protection/i);
});

test("temporary API upstream cooldown is localized as recoverable, not an OAuth ban", async () => {
  const { getAccountStatusBadgeStatus } = await import("./usageFormat.ts");
  assert.equal(getAccountStatusBadgeStatus({ status: "cooldown", cooldown_reason: "api_upstream_unavailable" }), "api_upstream_unavailable");
  assert.equal(getAccountStatusBadgeStatus({ status: "api_upstream_unavailable" }), "api_upstream_unavailable");
  for (const status of ["error", "unauthorized", "paused", "quota_paused", "active"]) {
    assert.equal(getAccountStatusBadgeStatus({ status, openai_responses_api: true, cooldown_reason: "api_upstream_unavailable" }), status);
  }
  const badge = source("../components/StatusBadge.tsx");
  assert.match(badge, /api_upstream_unavailable: \{ variant: 'secondary'/);
  assert.match(badge, /status.apiUpstreamUnavailableHint/);
  for (const locale of ["zh", "en", "zh-TW"]) {
    const status = JSON.parse(source(`../locales/${locale}.json`)).status;
    assert.ok(status.api_upstream_unavailable);
    assert.match(status.apiUpstreamUnavailableHint, /401\/403/);
    assert.match(status.apiUpstreamUnavailableHint, /OAuth/);
  }
  const accounts = source("../pages/Accounts.tsx");
  const countdown = accounts.split("function getAccountStatusCountdownUntil(")[1]?.split("function AccountStatusCountdown(")[0] ?? "";
  assert.match(countdown, /status === "api_upstream_unavailable"/);
});

test("unsampled status explains that ordinary traffic supplies health evidence", () => {
  const badge = source("../components/StatusBadge.tsx");
  assert.match(badge, /key === 'unsampled'/);
  assert.match(badge, /status.unsampledHint/);
  for (const locale of ["zh", "en", "zh-TW"]) {
    const status = JSON.parse(source(`../locales/${locale}.json`)).status;
    assert.ok(status.unsampledHint);
  }
  const en = JSON.parse(source("../locales/en.json")).status;
  assert.match(en.unsampledHint, /probes[\s\S]*unavailable[\s\S]*business requests[\s\S]*health/i);
});
