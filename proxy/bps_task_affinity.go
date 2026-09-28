package proxy

import (
	"context"
	"strconv"
	"time"

	"github.com/codex2api/auth"
)

type bpsTaskAffinityDiagnostic struct {
	TaskKey            string `json:"task_key"`
	Result             string `json:"result"`
	PreferredAccountID int64  `json:"preferred_account_id,omitempty"`
	SelectedAccountID  int64  `json:"selected_account_id,omitempty"`
	Persisted          bool   `json:"persisted"`
	// Store is where the binding lives: database, or local for heuristic
	// seeds when persist_heuristic_affinity is off.
	Store           string `json:"store,omitempty"`
	BindingRevision int64  `json:"binding_revision,omitempty"`
	revision        int64
	turnEpoch       string
}

// Turn isolation follows an authoritative migration segment first. Requests
// without a native session use their user-scoped soft affinity revision only
// for turn identity; this never promotes the preference to root ownership.
func bpsConvergenceTurnEpoch(ctx context.Context, account *auth.Account) string {
	if epoch := outboundEpochFromContext(ctx).identityKey(); epoch != "" {
		return epoch
	}
	state, _ := ctx.Value(inferredBPSSessionKey{}).(*inferredBPSSession)
	if state == nil || state.affinity == nil || account == nil || state.affinity.SelectedAccountID != account.ID() {
		return ""
	}
	d := state.affinity
	if d.Persisted && d.BindingRevision > 0 {
		if d.BindingRevision == 1 {
			return "" // No switch yet: retain the initial fixed task turn.
		}
		return codexIdentityDigest("bps-affinity-turn-epoch-v1", state.seed, strconv.FormatInt(d.BindingRevision, 10))
	}
	// A losing concurrent choice or failed persistence cannot reuse the winner's
	// turn. Keep an isolated request-local identity until a binding is available.
	return d.turnEpoch
}

// bpsTaskAffinityAccount reports whether account is a BPS account using a
// convergence mode with persistent task affinity.
func bpsTaskAffinityAccount(account *auth.Account, model string) bool {
	if account == nil || !bpsServesAccount(account, model) {
		return false
	}
	mode := account.CodexBPSConvergence()
	return mode == auth.CodexBPSConvergenceSession || mode == auth.CodexBPSConvergenceFull || mode == auth.CodexBPSConvergenceRound || mode == auth.CodexBPSConvergenceTurnRound
}

// bpsPreferredTaskAccount is the plugin's soft scheduling preference: the
// account previously associated with the pre-dispatch task seed. The outbound
// task_id itself is account-scoped and can only be resolved after selection.
// Never promote this heuristic into a fingerprint, owner, slot, or strict pin.
func (h *Handler) bpsPreferredTaskAccount(ctx context.Context, state *inferredBPSSession, model string) int64 {
	if state == nil || state.seed == "" || h == nil || h.store == nil || h.store.GetAffinityMode() == "off" {
		return 0
	}
	affinities, where := h.bpsTaskAffinityStoreFor(state)
	if affinities == nil {
		return 0
	}
	diagnostic := &bpsTaskAffinityDiagnostic{TaskKey: state.seed, Result: "new_task", Store: where, turnEpoch: NewUpstreamSessionUUID()}
	state.affinity = diagnostic
	lookupCtx, cancel := context.WithTimeout(ctx, time.Second)
	record, err := affinities.ReadBPSTaskAffinity(lookupCtx, codexIdentityDigest("bps-task-affinity-v1", state.seed))
	cancel()
	if err != nil {
		diagnostic.Result = "lookup_failed"
		return 0
	}
	diagnostic.PreferredAccountID = record.AccountID
	diagnostic.revision = record.Revision
	if !bpsTaskAffinityAccount(h.store.FindByID(record.AccountID), model) {
		return 0
	}
	return record.AccountID
}

func (h *Handler) rememberBPSTaskAccount(ctx context.Context, state *inferredBPSSession, account *auth.Account, model string) {
	if state == nil || state.affinity == nil || account == nil || state.affinity.Result == "lookup_failed" {
		return
	}
	affinities, where := h.bpsTaskAffinityStoreFor(state)
	if affinities == nil {
		return
	}
	diagnostic := state.affinity
	diagnostic.SelectedAccountID = account.ID()
	diagnostic.Persisted, diagnostic.BindingRevision = false, 0
	if !bpsTaskAffinityAccount(account, model) {
		diagnostic.Result = "route_not_bps_convergence"
		return
	}
	writeCtx, cancel := context.WithTimeout(ctx, time.Second)
	diagnostic.Store = where
	current, err := affinities.UpdateBPSTaskAffinity(writeCtx, codexIdentityDigest("bps-task-affinity-v1", state.seed), diagnostic.revision, account.ID())
	cancel()
	if err == nil && current.AccountID == account.ID() {
		diagnostic.BindingRevision, diagnostic.revision = current.Revision, current.Revision
	}
	switch {
	case err != nil:
		diagnostic.Result = "persist_failed"
	case current.AccountID != account.ID():
		diagnostic.Result = "concurrent_selection"
	case diagnostic.PreferredAccountID == account.ID():
		diagnostic.Result, diagnostic.Persisted = "reused", true
	case diagnostic.PreferredAccountID != 0:
		diagnostic.Result, diagnostic.Persisted = "switched", true
	default:
		diagnostic.Result, diagnostic.Persisted = "bound", true
	}
}
