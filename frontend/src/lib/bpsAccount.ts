import type { CodexBPSConvergence, CodexBPSProfile, UpdateAccountSchedulerRequest } from '../types'

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

// Tri-state account switches: inherit follows account group / global plugin
// switch (BPS) or "native unless BPS" (native).
export type BPSTriState = 'inherit' | 'on' | 'off'

export const BPS_PROFILES: CodexBPSProfile[] = ['word', 'excel', 'sheets', 'powerpoint']
export const BPS_CONVERGENCE_MODES: CodexBPSConvergence[] = ['off', 'session', 'full', 'round', 'turn_round']

export interface BPSAccountSource {
  codex_bps_enabled?: boolean | null
  codex_native_enabled?: boolean | null
  codex_native_models?: string[] | null
  codex_bps_models?: string[] | null
  codex_bps_image_trim_enabled?: boolean
  codex_bps_profile?: CodexBPSProfile | string
  codex_bps_convergence?: CodexBPSConvergence | string
}

export interface BPSAccountForm {
  enabled: BPSTriState
  native: BPSTriState
  nativeModels: string
  bpsModels: string
  imageTrim: boolean
  profile: CodexBPSProfile
  convergence: CodexBPSConvergence
}

function triState(value: boolean | null | undefined): BPSTriState {
  if (value === true) return 'on'
  if (value === false) return 'off'
  return 'inherit'
}

function triStateValue(value: BPSTriState): boolean | null {
  if (value === 'on') return true
  if (value === 'off') return false
  return null
}

export function bpsFormFromAccount(account: BPSAccountSource): BPSAccountForm {
  const profile = BPS_PROFILES.includes(account.codex_bps_profile as CodexBPSProfile)
    ? (account.codex_bps_profile as CodexBPSProfile)
    : 'word'
  const convergence = BPS_CONVERGENCE_MODES.includes(account.codex_bps_convergence as CodexBPSConvergence)
    ? (account.codex_bps_convergence as CodexBPSConvergence)
    : 'off'
  return {
    enabled: triState(account.codex_bps_enabled),
    native: triState(account.codex_native_enabled),
    nativeModels: (account.codex_native_models ?? []).join(', '),
    bpsModels: (account.codex_bps_models ?? []).join(', '),
    imageTrim: account.codex_bps_image_trim_enabled ?? false,
    profile,
    convergence,
  }
}

export function parseRouteModels(text: string): string[] {
  return text.split(/[\s,]+/).map(value => value.trim()).filter(Boolean)
}

// Only the override is sent for the quick configuration; the full dialog
// sends every BPS field.
export function bpsPayloadFromForm(form: BPSAccountForm, overrideOnly = false): Partial<UpdateAccountSchedulerRequest> {
  if (overrideOnly) return { codex_bps_enabled: triStateValue(form.enabled) }
  return {
    codex_bps_enabled: triStateValue(form.enabled),
    codex_native_enabled: triStateValue(form.native),
    codex_native_models: parseRouteModels(form.nativeModels),
    codex_bps_models: parseRouteModels(form.bpsModels),
    codex_bps_image_trim_enabled: form.imageTrim,
    codex_bps_profile: form.profile,
    codex_bps_convergence: form.convergence,
  }
}
