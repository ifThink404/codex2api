package proxy

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func recordFailoverContinuity(request *gin.Context, body []byte, state *sessionContinuityRequest, diagnostic *sessionAccountFailoverDiagnostic) bool {
	_, _, _, invalid := parseContinuityWindow(request.Request.Header, body, isResponsesWebSocketUpgradeRequest(request.Request))
	windowState := invalid
	if windowState == "" {
		windowState = "valid"
	}
	missing := CurrentRuntimeSettings().CodexForkAccountFallbackEnabled && state.Diagnostic.Mode != "enforce" &&
		invalid == "window_missing" && !state.Known &&
		(!state.Diagnostic.WouldBlock || state.Diagnostic.Result == "window_missing") &&
		(state.ThreadID == "" || state.Record.ThreadID == "" || state.ThreadID == state.Record.ThreadID)
	diagnostic.Continuity = &database.SessionFailoverContinuity{
		Mode: state.Diagnostic.Mode, Result: state.Diagnostic.Result, WouldBlock: state.Diagnostic.WouldBlock,
		WindowState: windowState, WindowKnown: state.Known, OwnerSource: state.Diagnostic.OwnerSource,
		PersistentAccountID: state.Record.AccountID, DeferredWindow: missing,
	}
	return missing
}

// A live scoped binding (or verified window grant) may predate continuity
// persistence. Seed that same owner before performing the normal generation-CAS
// switch. Never create an owner from client metadata or overwrite an existing one.
func (handler *Handler) recoverRelaxedFailoverOwner(request *gin.Context, key string, state *sessionContinuityRequest, owner *auth.Account, diagnostic *sessionAccountFailoverDiagnostic) string {
	if !CurrentRuntimeSettings().CodexForkAccountFallbackEnabled || handler.db == nil {
		return "persistent_owner_required"
	}
	if state.Diagnostic.OwnerSource != "live_binding" && state.Diagnostic.OwnerSource != "window_grant" {
		return "persistent_owner_required"
	}
	shard, _ := strconv.ParseUint(state.Key[:2], 16, 8)
	lock := &handler.continuityLocks[shard%uint64(len(handler.continuityLocks))]
	lock.Lock()
	defer lock.Unlock()
	ctx, cancel := context.WithTimeout(request.Request.Context(), time.Second)
	defer cancel()
	_, found, err := handler.db.ReadSessionContinuity(ctx, state.Key)
	if err != nil {
		diagnostic.Continuity.OwnerRecovery = "read_failed"
		return "ownership_unavailable"
	}
	if found {
		diagnostic.Continuity.OwnerRecovery = "record_changed"
		return "ownership_changed"
	}
	live, liveFound := handler.store.LiveSessionAccountID(key, time.Now())
	if liveFound && live != state.Diagnostic.OwnerAccount {
		diagnostic.Continuity.OwnerRecovery = "binding_changed"
		return "ownership_changed"
	}
	grant := windowGrantForRequest(request)
	ownerID := state.Diagnostic.OwnerAccount
	grantMatches := grant != nil && grant.Grant.OwnerKey == key && grant.Grant.OwnerAccountID == ownerID &&
		grant.Grant.ExpiresAt.After(time.Now()) && (grant.Grant.Confirmed || grant.Grant.PendingUntil.After(time.Now()))
	if !liveFound && !grantMatches || grant != nil && !grantMatches {
		diagnostic.Continuity.OwnerRecovery = "binding_unavailable"
		return "persistent_owner_required"
	}
	// If the historical route is missing, use a conservative BPS floor whenever
	// that account permits BPS. This avoids silently moving BPS history to native.
	mode := "native"
	if owner == nil || owner.CodexRouteAllows("bps", "", true) {
		mode = "bps"
	}
	record := database.SessionContinuityRecord{AccountID: ownerID, UpstreamMode: mode, LastSeen: state.StartedAt}
	if state.Known {
		record.ThreadID, record.Number, record.NumberKnown = state.ThreadID, state.Number, true
	}
	committed, err := handler.db.CommitSessionContinuity(ctx, state.Key, record)
	if err != nil {
		diagnostic.Continuity.OwnerRecovery = "commit_failed"
		if errors.Is(err, database.ErrSessionOwnerConflict) {
			return "ownership_changed"
		}
		return "ownership_unavailable"
	}
	state.Record = committed
	handler.cacheSessionContinuity(state.Key, sessionContinuityCacheEntry{Record: committed, CheckedAt: time.Now(), WrittenAt: time.Now()})
	handler.attachSessionOutboundEpoch(request, state.Key, committed)
	diagnostic.PreviousUpstreamMode = normalizedCodexRoute(committed.UpstreamMode)
	diagnostic.Continuity.OwnerRecovery = "persisted_existing_binding"
	return ""
}
