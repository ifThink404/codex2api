package proxy

import (
	"time"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

type groupRoutingDiagnostic struct {
	Reason    string `json:"reason"`
	AccountID int64  `json:"account_id,omitempty"`
	groups    []int64
	blocked   bool
}

func chatCompletionsGroupRouting(request *gin.Context) bool {
	return request != nil && request.Request != nil && request.Request.URL != nil && request.Request.URL.Path == "/v1/chat/completions"
}

// Resolve the route before API-relay exemption and dispatch inspect the account
// pool. Looking up the scoped owner never grants access or changes its binding.
func (handler *Handler) prepareChatGroupRouting(request *gin.Context, identity requestSessionIdentity) {
	if !chatCompletionsGroupRouting(request) {
		return
	}
	state := usageRequestDiagnosticState(request)
	state.GroupRouting = nil
	if CurrentRuntimeSettings().CodexForkAccountFallbackEnabled {
		state.GroupRouting = &groupRoutingDiagnostic{Reason: "relaxed_no_groups"}
		return
	}
	row := apiKeyRowFromContext(request)
	if row == nil || len(int64GroupSet(row.Limits.NoAffinityGroupIDs)) == 0 {
		return
	}
	decision := &groupRoutingDiagnostic{Reason: "chat_completions_path"}
	state.GroupRouting = decision
	if !identity.stableIdentity || identity.unlinkedFallbackOnly {
		return
	}
	keyID := requestAPIKeyID(request)
	for _, original := range []string{identity.affinityID, identity.forkSourceAffinityID} {
		if original == "" {
			continue
		}
		key := sessionAffinityKey(original, keyID)
		entry, found, err := handler.readSessionContinuity(request.Request.Context(), hashRiskIdentity(key))
		if err != nil {
			decision.Reason, decision.blocked = "group_routing_owner_lookup_failed", true
			return
		}
		owner := entry.Record.AccountID
		if !found {
			owner, _ = handler.store.LiveSessionAccountID(key, time.Now())
			if owner == 0 {
				source := identity
				source.affinityID = original
				owner, _ = handler.store.LiveSessionAccountID(capacityAwareSessionAffinityKey(apiRelaySessionIdentity(source), keyID), time.Now())
			}
		}
		if owner <= 0 {
			continue
		}
		decision.Reason, decision.AccountID = "existing_session_binding", owner
		account := handler.store.FindByID(owner)
		if account == nil {
			decision.Reason, decision.blocked = "group_routing_owner_unavailable", true
			return
		}
		// Keep the existing cohort. Any actual failover still has to satisfy its
		// own group, tag, model, credential, capacity and context checks.
		decision.groups = account.GroupIDSnapshot()
		return
	}
}

func applyChatGroupRouting(request *gin.Context, filter auth.AccountFilter, splitGroups map[int64]struct{}) auth.AccountFilter {
	state := usageRequestDiagnosticState(request)
	if state.GroupRouting == nil {
		state.GroupRouting = &groupRoutingDiagnostic{Reason: "chat_completions_path"}
	}
	decision := state.GroupRouting
	if decision.blocked {
		return func(*auth.Account) bool {
			selectionTraceForRequest(request).Reject(decision.Reason)
			return false
		}
	}
	if decision.AccountID > 0 {
		return func(account *auth.Account) bool {
			if !account.HasExactGroupIDs(decision.groups) {
				selectionTraceForRequest(request).Reject("affinity_group_mismatch")
				return false
			}
			return filter == nil || filter(account)
		}
	}
	return groupMembershipFilter(splitGroups, true, filter, selectionTraceForRequest(request))
}
