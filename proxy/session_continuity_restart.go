package proxy

import (
	"context"
	"errors"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func (handler *Handler) prepareContinuityRestart(request *gin.Context, body []byte) *api.APIError {
	if handler.db == nil {
		return sessionContinuityError("ownership_unavailable")
	}
	_, _, cleanup, err := cleanSessionRestartContext(sessionFailoverRequestHeaders(request), body, nil, PreserveSessionInput(request.Request.Context()))
	if err != nil {
		if failure := sessionToolPreservationAPIError(err, cleanup); failure != nil {
			return failure
		}
		failure := api.NewAPIError("codex_session_restart_context_required", "重建出站会话后没有可用输入，请补充当前问题和必要资料，或新开对话。", api.ErrorTypeInvalidRequest)
		failure.Details = gin.H{"retry": "stop", "context_cleanup": cleanup}
		return failure
	}
	return nil
}

// Called under the existing per-root continuity lock, after window admission.
func (handler *Handler) commitContinuityRestart(request *gin.Context, account *auth.Account, state *sessionContinuityRequest) *api.APIError {
	if handler.db == nil || account.IsRelayStyle() || account.EffectiveAccountID() == "" {
		return sessionContinuityError("ownership_unavailable")
	}
	next := state.Record
	if next.AccountID == 0 {
		info := codexRouteRequestInfo(request.Request.Context())
		floor, _ := request.Request.Context().Value(codexRouteFloorKey{}).(string)
		next.UpstreamMode = selectCodexRoute(account, info.Model, floor, info.Auxiliary)
		if next.UpstreamMode == "" {
			return sessionModelUnavailableError(request)
		}
	}
	next.AccountID, next.ThreadID, next.Number, next.NumberKnown = account.ID(), state.ThreadID, state.Number, true
	forkFallbackRestartRecord(&next, request)
	next.LastSeen, next.LastFailoverReason = state.StartedAt, "continuity_"+state.RestartReason
	ctx, cancel := context.WithTimeout(request.Request.Context(), time.Second)
	defer cancel()
	committed, err := handler.db.RestartSessionContinuity(ctx, state.Key, state.Record, next)
	if err != nil {
		if errors.Is(err, database.ErrSessionOwnerConflict) {
			return sessionContinuityError("owner_conflict")
		}
		return sessionContinuityError("ownership_unavailable")
	}
	handler.cacheSessionContinuity(state.Key, sessionContinuityCacheEntry{Record: committed, CheckedAt: time.Now(), WrittenAt: time.Now()})
	state.Record, state.Admitted = committed, true
	state.Diagnostic.Action, state.Diagnostic.OwnerAccount = "restarted", account.ID()
	diagnostic := &sessionAccountFailoverDiagnostic{Result: "restarted", Phase: "after_restart", Reason: committed.LastFailoverReason, TriggerReason: committed.LastFailoverReason, PreviousAccountID: committed.PreviousAccountID, AccountID: committed.AccountID, Generation: committed.FailoverCount}
	usageRequestDiagnosticState(request).AccountFailover = diagnostic
	state.Diagnostic.AccountFailover = diagnostic
	handler.attachSessionOutboundEpoch(request, state.Key, committed)
	selectionTraceForRequest(request).PinAccount(account.ID())
	return nil
}
