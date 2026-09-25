package proxy

import (
	"context"
	"time"

	"github.com/codex2api/auth"
)

type bpsTaskAffinityDiagnostic struct {
	TaskKey            string `json:"task_key"`
	Result             string `json:"result"`
	PreferredAccountID int64  `json:"preferred_account_id,omitempty"`
	SelectedAccountID  int64  `json:"selected_account_id,omitempty"`
	Persisted          bool   `json:"persisted"`
	revision           int64
}

func bpsTaskAffinityAccount(account *auth.Account, model string) bool {
	if account == nil || selectCodexRoute(account, model, "", false) != "bps" {
		return false
	}
	mode := account.EffectiveCodexFingerprintMode()
	return mode == auth.CodexFingerprintModeSession || mode == auth.CodexFingerprintModeFull || mode == auth.CodexFingerprintModeRound
}

// Prefer the account associated with the pre-dispatch task seed. The outbound
// task_id itself is account-scoped and can only be resolved after selection.
// Never promote this heuristic into a fingerprint, owner, slot, or strict pin.
func (h *Handler) nextAccountForBPSTask(ctx context.Context, affinityKey string, apiKeyID int64, exclude map[int64]bool, filter auth.AccountFilter, policy auth.DispatchPolicy) (*auth.Account, string, auth.SessionAffinityGuard) {
	state, _ := ctx.Value(inferredBPSSessionKey{}).(*inferredBPSSession)
	trace := auth.SelectionTraceFromContext(ctx)
	if state == nil || state.seed == "" || h.db == nil || trace.PinnedAccount() > 0 || h.store.GetAffinityMode() == "off" {
		return h.nextAccountForSessionWithDispatchGuard(affinityKey, apiKeyID, exclude, filter, policy, trace)
	}
	diagnostic := &bpsTaskAffinityDiagnostic{TaskKey: state.seed, Result: "new_task"}
	state.affinity = diagnostic
	key := codexIdentityDigest("bps-task-affinity-v1", state.seed)
	lookupCtx, cancel := context.WithTimeout(ctx, time.Second)
	record, err := h.db.ReadBPSTaskAffinity(lookupCtx, key)
	cancel()
	if err != nil {
		diagnostic.Result = "lookup_failed"
		return h.nextAccountForSessionWithDispatchGuard(affinityKey, apiKeyID, exclude, filter, policy, trace)
	}
	diagnostic.PreferredAccountID = record.AccountID
	diagnostic.revision = record.Revision
	preferred := h.store.FindByID(record.AccountID)
	if bpsTaskAffinityAccount(preferred, policy.Model()) {
		account := h.store.TakePreferredAccountWithDispatch(record.AccountID, apiKeyID, exclude, filter, policy, trace)
		if account != nil {
			if h.store.AdmitAccountSession(account, affinityKey, time.Now(), trace) {
				diagnostic.Result, diagnostic.SelectedAccountID, diagnostic.Persisted = "reused", account.ID(), true
				return account, account.GetProxyURL(), auth.SessionAffinityGuard{}
			}
			h.store.Release(account)
		}
	}
	account, proxyURL, guard := h.nextAccountForSessionWithDispatchGuard(affinityKey, apiKeyID, exclude, filter, policy, trace)
	if account == nil {
		diagnostic.Result = "no_available_account"
		return account, proxyURL, guard
	}
	h.rememberBPSTaskAccount(ctx, account, policy)
	return account, proxyURL, guard
}

func (h *Handler) rememberBPSTaskAccount(ctx context.Context, account *auth.Account, policy auth.DispatchPolicy) {
	state, _ := ctx.Value(inferredBPSSessionKey{}).(*inferredBPSSession)
	if state == nil || state.affinity == nil || h.db == nil || account == nil || state.affinity.Result == "lookup_failed" {
		return
	}
	diagnostic := state.affinity
	diagnostic.SelectedAccountID = account.ID()
	if !bpsTaskAffinityAccount(account, policy.Model()) {
		diagnostic.Result = "route_not_bps_convergence"
		return
	}
	writeCtx, cancel := context.WithTimeout(ctx, time.Second)
	current, err := h.db.UpdateBPSTaskAffinity(writeCtx, codexIdentityDigest("bps-task-affinity-v1", state.seed), diagnostic.revision, account.ID())
	cancel()
	switch {
	case err != nil:
		diagnostic.Result = "persist_failed"
	case current.AccountID != account.ID():
		diagnostic.Result = "concurrent_selection"
	case diagnostic.PreferredAccountID != 0:
		diagnostic.Result, diagnostic.Persisted = "switched", true
	default:
		diagnostic.Result, diagnostic.Persisted = "bound", true
	}
}
