package proxy

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type sessionAccountFailoverContextKey struct{}

type sessionAccountFailoverDiagnostic = database.SessionAccountFailoverDiagnostic

func sessionFailoverContextError(request *gin.Context, diagnostic *sessionAccountFailoverDiagnostic, block string) *api.APIError {
	if block == "missing_request_context" {
		if blockers := preservedToolPairingBlockers(diagnostic.ContextCleanup); len(blockers) > 0 {
			block, diagnostic.ContextBlockers = "incomplete_tool_context", blockers
		}
	}
	continuityRestart := strings.HasPrefix(diagnostic.TriggerReason, "continuity_") || strings.HasPrefix(diagnostic.Reason, "continuity_")
	diagnostic.Result, diagnostic.Reason, diagnostic.BlockReason = "blocked", block, block
	state := usageRequestDiagnosticState(request)
	state.AccountFailover = diagnostic
	if state.Continuity != nil {
		state.Continuity.AccountFailover = diagnostic
	}
	reason := "会话归属无法核实"
	switch block {
	case "upstream_continuation":
		reason = "携带旧 previous_response_id 或 conversation 续链"
	case "connection_turn_state":
		reason = "携带无法确认属于目标账号的 turn-state"
	case "opaque_upstream_context":
		reason = "携带无法迁移的加密内容、文件或输入项引用"
	case "missing_request_context":
		reason = "缺少完整请求上下文"
	case "incomplete_tool_context":
		reason = "工具结果缺少对应的工具调用"
	case "persistent_owner_required":
		reason = "缺少一致的持久化账号归属"
	case "ownership_changed":
		reason = "账号归属已变化，请重新发起请求"
	case "ownership_unavailable":
		reason = "暂时无法读取或保存账号归属"
	case "invalid_session_continuity":
		reason = "会话窗口连续性校验未通过"
	case "window_identity_required":
		reason = "缺少有效的会话窗口标识"
	}
	if len(diagnostic.ContextBlockers) > 0 {
		first := diagnostic.ContextBlockers[0]
		labels := map[string]string{"encrypted_content": "加密上下文", "file_id": "上游文件引用", "item_reference": "上游输入项引用"}
		if label := labels[first.Kind]; label != "" {
			reason = "携带不能确认可迁移的" + label
		}
		reason += "（" + first.Path
		if first.ItemType != "" {
			reason += "，类型 " + first.ItemType
		}
		reason += "）"
	}
	guidance := "请恢复完整未加密上下文，或新开对话。"
	if block == "persistent_owner_required" {
		guidance = "请恢复原会话账号绑定，或新开对话。"
	}
	message := "绑定账号不可用，当前请求不能安全换号：" + reason + "。" + guidance
	if diagnostic.Phase == "after_switch" {
		message = "会话已更换绑定账号，当前请求不能安全续接：" + reason + "。" + guidance
	}
	if continuityRestart {
		message = "会话已重建出站上下文，当前请求不能续接：" + reason + "。" + guidance
	}
	request.Header("X-Should-Retry", "false")
	return api.NewAPIErrorWithDetails("codex_session_failover_context_required", message, api.ErrorTypeInvalidRequest,
		gin.H{"reason": block, "trigger_reason": diagnostic.TriggerReason, "phase": diagnostic.Phase, "retry": "stop", "context_blockers": diagnostic.ContextBlockers, "context_cleanup": diagnostic.ContextCleanup})
}

type sessionAccountFailoverPlan struct {
	MissingOwner  *missingSessionOwner
	Failure       *api.APIError
	PreserveInput bool
	Request       *gin.Context
	Key           string
	Body          []byte
	Checked       bool
	Diagnostic    *sessionAccountFailoverDiagnostic
}

func (handler *Handler) validateMigratedSessionContext(request *gin.Context, body []byte, record database.SessionContinuityRecord, rootKeys ...string) *api.APIError {
	if record.LossyContextRestart {
		epoch := outboundEpochFromContext(request.Request.Context())
		if epoch == nil || epoch.record.AccountID != record.AccountID || epoch.record.FailoverCount != record.FailoverCount {
			return sessionContinuityError("owner_conflict")
		}
		known, cancel := epoch.restartContextVerifier(request.Request.Context())
		defer cancel()
		_, _, report, err := cleanSessionRestartContext(sessionFailoverRequestHeaders(request), body, known, record.PreserveRestartInput)
		if err != nil {
			if failure := sessionToolPreservationAPIError(err, report); failure != nil {
				return failure
			}
			diagnostic := &sessionAccountFailoverDiagnostic{Phase: "after_switch", TriggerReason: record.LastFailoverReason, PreviousAccountID: record.PreviousAccountID, AccountID: record.AccountID, Generation: record.FailoverCount, ContextCleanup: report}
			failure := sessionFailoverContextError(request, diagnostic, "missing_request_context")
			failure.Message = err.Error()
			if typed, ok := err.(*Error); ok {
				failure.Message = typed.Message
			}
			return failure
		}
		return nil
	}
	known, cancelKnown := handler.sessionContextVerifier(request, record, rootKeys...)
	defer cancelKnown()
	blocked, blockers := inspectSessionFailoverContext(sessionFailoverRequestHeaders(request), body, known)
	if blocked == "missing_request_context" {
		return nil
	}
	if blocked == "upstream_continuation" && gjson.GetBytes(body, "previous_response_id").String() != "" && !gjson.GetBytes(body, "conversation").Exists() && !gjson.GetBytes(body, "conversation_id").Exists() {
		owner := responseCacheOwnerForRequest(request, requestAPIKeyID(request))
		lookup, cancel := context.WithTimeout(request.Request.Context(), time.Second)
		defer cancel()
		affinity, found := lookupResponseAccountAffinity(lookup, handler.cache, owner, gjson.GetBytes(body, "previous_response_id").String())
		segmentMatches := !record.OutboundWindowReset
		if record.OutboundWindowReset && len(rootKeys) > 0 {
			segmentMatches = affinity.OutboundSegment == codexIdentityDigest("codex-outbound-segment-v1", hashRiskIdentity(rootKeys[0]), strconv.FormatUint(record.FailoverCount, 10))
		}
		if found && affinity.AccountID == record.AccountID && segmentMatches {
			withoutPrevious, _ := sjson.DeleteBytes(body, "previous_response_id")
			blocked, blockers = inspectSessionFailoverContext(sessionFailoverRequestHeaders(request), withoutPrevious, known)
			if blocked == "missing_request_context" || blocked == "incomplete_tool_context" {
				blocked = ""
			}
		}
	}
	if blocked != "" {
		return sessionFailoverContextError(request, &sessionAccountFailoverDiagnostic{
			Phase: "after_switch", TriggerReason: record.LastFailoverReason,
			PreviousAccountID: record.PreviousAccountID, AccountID: record.AccountID, Generation: record.FailoverCount,
			ContextBlockers: blockers,
		}, blocked)
	}
	return nil
}

func sessionFailoverRequestHeaders(request *gin.Context) http.Header {
	if isResponsesWebSocketUpgradeRequest(request.Request) {
		return nil
	}
	return request.Request.Header
}

func (handler *Handler) restoreMigratedSessionOwner(request *gin.Context, key string, body []byte) *api.APIError {
	if root, related := auth.RelatedSessionRootKey(key); related {
		if backgroundAccountMatchFromContext(request.Request.Context()) == nil {
			handler.attachSessionOutboundEpoch(request, "", database.SessionContinuityRecord{})
		}
		return handler.prepareBackgroundAccountMatch(request, root, body)
	}
	handler.attachSessionOutboundEpoch(request, "", database.SessionContinuityRecord{})
	request.Request = request.Request.WithContext(context.WithValue(request.Request.Context(), backgroundAccountMatchContextKey{}, (*backgroundAccountMatch)(nil)))
	usageRequestDiagnosticState(request).BackgroundAccountMatch = nil
	if handler.db == nil || key == "" {
		return nil
	}
	entry, found, err := handler.readSessionContinuity(request.Request.Context(), hashRiskIdentity(key))
	if err != nil {
		return sessionContinuityError("ownership_unavailable")
	}
	if !found {
		return nil
	}
	handler.attachSessionOutboundEpoch(request, hashRiskIdentity(key), entry.Record)
	if entry.Record.FailoverCount == 0 {
		return nil
	}
	if err := handler.validateMigratedSessionContext(request, body, entry.Record, key); err != nil {
		return err
	}
	selectionTraceForRequest(request).PinAccount(entry.Record.AccountID)
	recordUsageRootAccount(request, entry.Record.AccountID, true)
	return nil
}

func (handler *Handler) recoverSessionFailoverGrant(request *gin.Context, key string) error {
	if windowGrantForRequest(request) != nil || handler.db == nil {
		return nil
	}
	status, identity := handler.cachedNewAPIPolicyAuditState(request)
	if (status != "verified" && status != "signed_response") || !identity.MetaVerified || identity.Identity.UserID == "" {
		return nil
	}
	subject := cache.PromptSessionLimitSubject(identity.Platform, identity.Identity.UserID)
	state, err := handler.db.ReadUserWindowAdmissions(request.Request.Context(), subject)
	if err != nil {
		return err
	}
	var selected *database.UserWindowGrant
	for _, grant := range state.Windows {
		if grant != nil && grant.OwnerKey == key {
			if selected != nil || grant.Expanded || !grant.Confirmed || !grant.ExpiresAt.After(time.Now()) {
				return errWindowGrantRefresh
			}
			selected = grant
		}
	}
	if selected != nil {
		request.Set(windowGrantContextKey, &signedWindowGrant{Version: 1, Platform: identity.Platform, UserID: identity.Identity.UserID, APIKeyID: identity.APIKeyID, Fingerprint: identity.Meta.RootSessionFingerprint, Grant: *selected})
	}
	return nil
}

func sessionAccountFailoverReason(account *auth.Account, policy auth.DispatchPolicy) string {
	if account == nil {
		return ""
	}
	if account.UsageLimitBypassEligible(policy) {
		return ""
	}
	if policy.IsSpark() {
		if account.SparkDispatchEligible() {
			return ""
		}
		if account.SparkDispatchUsageLimited() {
			return "account_spark_usage_exhausted"
		}
	}
	return account.SessionAccountFailoverReason()
}

func (handler *Handler) sessionFailoverReasonForRequest(request *gin.Context, account *auth.Account, key string, policy auth.DispatchPolicy) string {
	if reason := sessionAccountFailoverReason(account, policy); reason != "" {
		return reason
	}
	if reason := codexRouteFailureForRequest(request.Request.Context(), account); reason != "" {
		return reason
	}
	if account != nil && account.SessionCapacityLimits().Enabled && !handler.store.CanAdmitAccountSession(account, key, time.Now(), selectionTraceForRequest(request)) {
		return "account_session_capacity_full"
	}
	return ""
}

func sessionFailoverContextBlock(headers http.Header, body []byte) string {
	return sessionFailoverContextBlockWithVerifier(headers, body, nil)
}

func sessionAccountFailoverEnabledBy(reason string) string {
	settings := CurrentRuntimeSettings()
	if settings.CodexSessionFailoverEnabled {
		return "session_failover"
	}
	if settings.CodexForkAccountFallbackEnabled {
		switch reason {
		case "account_disabled", "account_paused", "account_usage_exhausted", "account_spark_usage_exhausted", "account_session_capacity_full", "account_payment_required", "account_missing":
			return "relaxed_mode"
		}
	}
	return ""
}

func (handler *Handler) prepareSessionAccountFailover(request *gin.Context, key string, body []byte, policy auth.DispatchPolicy) (bool, *api.APIError) {
	state := continuityRequest(request)
	if state == nil || state.Diagnostic == nil || state.Diagnostic.OwnerAccount <= 0 {
		return false, nil
	}
	owner := handler.store.FindByID(state.Diagnostic.OwnerAccount)
	var missing *missingSessionOwner
	lookup := ""
	if owner == nil && CurrentRuntimeSettings().CodexForkAccountFallbackEnabled {
		var err error
		owner, missing, lookup, err = handler.resolveMissingSessionOwner(request.Request.Context(), state.Diagnostic.OwnerAccount)
		if err != nil {
			diagnostic := &sessionAccountFailoverDiagnostic{PreviousAccountID: state.Diagnostic.OwnerAccount, TriggerReason: "account_missing", EnabledBy: "relaxed_mode", Phase: "before_switch", OwnerLookup: lookup}
			recordFailoverContinuity(request, body, state, diagnostic)
			return false, sessionFailoverContextError(request, diagnostic, "ownership_unavailable")
		}
	}
	reason := handler.sessionFailoverReasonForRequest(request, owner, key, policy)
	if missing != nil {
		reason = "account_missing"
	}
	if reason == "" {
		if lookup == "reloaded" {
			usageRequestDiagnosticState(request).AccountFailover = &sessionAccountFailoverDiagnostic{Result: "owner_reloaded", AccountID: owner.ID(), OwnerLookup: lookup, Generation: state.Record.FailoverCount}
		}
		return false, nil
	}
	diagnostic := &sessionAccountFailoverDiagnostic{PreviousUpstreamMode: normalizedCodexRoute(state.Record.UpstreamMode), Result: "blocked", Reason: reason, TriggerReason: reason, Phase: "before_switch", PreviousAccountID: state.Diagnostic.OwnerAccount, Generation: state.Record.FailoverCount, OwnerLookup: lookup}
	windowMissing := recordFailoverContinuity(request, body, state, diagnostic)
	if state.Record.AccountID == 0 {
		diagnostic.PreviousUpstreamMode = "" // Missing persistence is not evidence of a native route.
	}
	state.Diagnostic.AccountFailover = diagnostic
	usageRequestDiagnosticState(request).AccountFailover = diagnostic
	diagnostic.EnabledBy = sessionAccountFailoverEnabledBy(reason)
	if diagnostic.EnabledBy == "" {
		diagnostic.Result, diagnostic.BlockReason = "disabled", "failover_disabled"
		return false, nil
	}
	preserveInput := CurrentRuntimeSettings().CodexSessionFailoverPreserveInput || state.Record.PreserveRestartInput
	cleaned, cleanedHeaders, cleanup, cleanupError := cleanSessionRestartContext(sessionFailoverRequestHeaders(request), body, nil, preserveInput)
	diagnostic.ContextCleanup = cleanup
	if failure := sessionToolPreservationAPIError(cleanupError, cleanup); failure != nil {
		return false, failure
	}
	block, blockers := inspectSessionFailoverContext(cleanedHeaders, cleaned, nil)
	if preserveInput && cleanupError == nil {
		// Full replay intentionally retains opaque input; the preserving cleaner
		// has already rejected unresolved continuation handles and broken tool pairs.
		block, blockers = "", nil
	}
	if cleanupError != nil {
		block, blockers = "missing_request_context", nil
	}
	diagnostic.ContextBlockers = blockers
	if handler.db == nil || state.Record.AccountID != 0 && state.Record.AccountID != diagnostic.PreviousAccountID {
		block = "persistent_owner_required"
		diagnostic.ContextBlockers = nil
	} else if state.Diagnostic.WouldBlock && !windowMissing {
		block = "invalid_session_continuity"
		diagnostic.ContextBlockers = nil
	} else if (!state.Known || state.ThreadID == "") && !windowMissing {
		block = "window_identity_required"
		diagnostic.ContextBlockers = nil
	}
	if block != "" {
		failure := sessionFailoverContextError(request, diagnostic, block)
		if cleanupError != nil && block == "missing_request_context" {
			failure.Message = cleanupError.Error()
			if typed, ok := cleanupError.(*Error); ok {
				failure.Message = typed.Message
			}
		}
		return false, failure
	}
	if err := handler.recoverSessionFailoverGrant(request, key); err != nil {
		diagnostic.Reason = "window_grant_unavailable"
		return false, requestWindowGrantAPIError(err)
	}
	if state.Record.AccountID == 0 {
		if recoveryBlock := handler.recoverRelaxedFailoverOwner(request, key, state, owner, diagnostic); recoveryBlock != "" {
			return false, sessionFailoverContextError(request, diagnostic, recoveryBlock)
		}
	}
	diagnostic.Result = "pending"
	plan := &sessionAccountFailoverPlan{Request: request, Key: key, Body: body, Diagnostic: diagnostic, PreserveInput: preserveInput, MissingOwner: missing}
	request.Request = request.Request.WithContext(context.WithValue(request.Request.Context(), sessionAccountFailoverContextKey{}, plan))
	return true, nil
}

func (handler *Handler) takeSessionAccountFailover(ctx context.Context, key string, apiKeyID int64, exclude map[int64]bool, filter auth.AccountFilter, policy auth.DispatchPolicy) (*auth.Account, string, bool) {
	plan, _ := ctx.Value(sessionAccountFailoverContextKey{}).(*sessionAccountFailoverPlan)
	if plan != nil && plan.Key == key && plan.Failure != nil {
		return nil, "", true
	}
	if plan == nil || plan.Checked || plan.Key != key {
		return nil, "", false
	}
	plan.Checked = true
	if sessionAccountFailoverEnabledBy(plan.Diagnostic.TriggerReason) == "" || plan.Diagnostic.TriggerReason == "account_missing" && !CurrentRuntimeSettings().CodexForkAccountFallbackEnabled {
		plan.Diagnostic.Result = "disabled"
		plan.Diagnostic.BlockReason = "failover_disabled"
		return nil, "", false
	}
	request := plan.Request
	state := continuityRequest(request)
	oldID := plan.Diagnostic.PreviousAccountID
	old := handler.store.FindByID(oldID)
	if old == nil && CurrentRuntimeSettings().CodexForkAccountFallbackEnabled {
		var err error
		old, plan.MissingOwner, plan.Diagnostic.OwnerLookup, err = handler.resolveMissingSessionOwner(ctx, oldID)
		if err != nil {
			plan.Failure = sessionFailoverContextError(request, plan.Diagnostic, "ownership_unavailable")
			return nil, "", true
		}
	}
	if state == nil || old == nil && plan.MissingOwner == nil || old != nil && handler.sessionFailoverReasonForRequest(request, old, key, policy) == "" {
		plan.Diagnostic.Result = "owner_recovered"
		return nil, "", false
	}
	if handler.sessionBlacklistError(request) != nil || ctx.Err() != nil {
		plan.Diagnostic.Result = "blocked"
		return nil, "", true
	}
	shard, _ := strconv.ParseUint(state.Key[:2], 16, 8)
	lock := &handler.continuityLocks[shard%uint64(len(handler.continuityLocks))]
	lock.Lock()
	defer lock.Unlock()
	entry, found, err := handler.readSessionContinuity(ctx, state.Key)
	if err != nil || !found || entry.Record.AccountID != oldID || entry.Record.FailoverCount != state.Record.FailoverCount {
		plan.Diagnostic.Result, plan.Diagnostic.Reason = "blocked", "ownership_changed"
		return nil, "", true
	}
	excluded := make(map[int64]bool, len(exclude)+1)
	for accountID, value := range exclude {
		excluded[accountID] = value
	}
	excluded[oldID] = true
	trace := &auth.SelectionTrace{}
	trace.EnableCandidateDetails()
	trace.SetExpandedWindow(selectionTraceForRequest(request).ExpandedWindow())
	defer func() {
		for _, reason := range trace.Snapshot().Reasons {
			selectionTraceForRequest(request).Reject(reason)
		}
	}()
	var ownerGroups []int64
	var oldUpstreamID string
	groupsKnown := true
	if old != nil {
		ownerGroups, oldUpstreamID = old.GroupIDSnapshot(), old.EffectiveAccountID()
	} else {
		ownerGroups, oldUpstreamID, groupsKnown = plan.MissingOwner.Groups, plan.MissingOwner.UpstreamID, plan.MissingOwner.GroupsKnown
	}
	selection := &database.SessionFailoverSelection{MatchMode: "exact_groups"}
	if !groupsKnown {
		selection.MatchMode = "request_scope_missing_owner"
	}
	selection.RequiredGroupIDs, _, selection.Truncated = failoverSelectionLabels(request, ownerGroups, nil)
	plan.Diagnostic.Selection = selection
	defer func() {
		details := trace.CandidateDetails()
		selection.RejectionCounts, selection.OmittedObservations = details.RejectionCounts, details.OmittedObservations
		selection.SchedulerIncomplete = trace.Snapshot().Incomplete
		for _, sample := range details.Samples {
			item := database.SessionFailoverCandidate{AccountID: sample.AccountID, Reason: sample.Reason}
			if account := handler.store.FindByID(sample.AccountID); account != nil {
				var truncated bool
				item.GroupIDs, item.Tags, truncated = failoverSelectionLabels(request, account.GroupIDSnapshot(), account.TagSnapshot())
				selection.Truncated = selection.Truncated || truncated
			}
			selection.Candidates = append(selection.Candidates, item)
		}
	}()
	eligible := func(account *auth.Account) bool {
		info := codexRouteRequestInfo(ctx)
		prior := entry.Record.UpstreamMode
		if floor, _ := ctx.Value(codexRouteFloorKey{}).(string); floor == "bps" {
			prior = "bps"
		}
		mode := selectCodexRoute(account, info.Model, prior, info.Auxiliary)
		if account.ID() == oldID && mode != "bps" {
			return false
		}
		if mode == "" {
			trace.RejectAccount(account.ID(), "upstream_mode_mismatch")
			return false
		}
		if groupsKnown && !account.HasExactGroupIDs(ownerGroups) {
			trace.RejectAccount(account.ID(), "account_groups_mismatch")
			return false
		}
		grant := windowGrantForRequest(request)
		limits := account.SessionCapacityLimits()
		if grant != nil && (grant.Grant.Expanded && !limits.Enabled || grant.Grant.NoWindow && limits.Enabled) {
			trace.RejectAccount(account.ID(), "window_grant_capacity_mismatch")
			return false
		}
		if account.IsRelayStyle() || account.EffectiveAccountID() == "" || account.ID() != oldID && account.EffectiveAccountID() == oldUpstreamID {
			trace.RejectAccount(account.ID(), "account_identity_ineligible")
			return false
		}
		if filter != nil && !filter(account) {
			trace.RejectAccount(account.ID(), "request_filter_mismatch")
			return false
		}
		if !handler.store.CanAdmitAccountSession(account, key, time.Now(), trace) {
			trace.RejectAccount(account.ID(), "session_capacity_exhausted")
			return false
		}
		return true
	}
	tryOriginalBPS := old != nil && normalizedCodexRoute(entry.Record.UpstreamMode) == "native" && codexRouteFailureForRequest(ctx, old) != "" && sessionAccountFailoverReason(old, policy) == ""
	for range 16 {
		selection.Attempts++
		var candidate *auth.Account
		if tryOriginalBPS {
			tryOriginalBPS = false
			plan.Diagnostic.OriginalAccountBPSAttempted = true
			if !exclude[oldID] {
				candidate = handler.store.NextExcludingWithDispatch(apiKeyID, exclude, func(a *auth.Account) bool { return a.ID() == oldID && eligible(a) }, policy, trace)
			}
		}
		if candidate == nil {
			candidate = handler.store.NextExcludingWithDispatch(apiKeyID, excluded, eligible, policy, trace)
		}
		if candidate == nil {
			break
		}
		excluded[candidate.ID()] = true
		info := codexRouteRequestInfo(ctx)
		prior := entry.Record.UpstreamMode
		if floor, _ := ctx.Value(codexRouteFloorKey{}).(string); floor == "bps" {
			prior = "bps"
		}
		targetMode := selectCodexRoute(candidate, info.Model, prior, info.Auxiliary)
		if targetMode == "" {
			handler.store.Release(candidate)
			continue
		}
		preview := entry.Record
		preview.UpstreamMode = targetMode
		preview.AccountID, preview.FailoverCount = candidate.ID(), entry.Record.FailoverCount+1
		preview.OutboundWindowReset = true
		preview.OutboundWindowBases = nil
		if state.Known {
			preview.OutboundWindowBases = map[string]uint64{state.ThreadID: state.Number}
		}
		previewContext := context.WithValue(ctx, sessionOutboundEpochContextKey{}, &sessionOutboundEpoch{handler: handler, key: state.Key, record: preview, preview: true})
		fingerprint := NewCodexTransportFingerprint(candidate, sessionFailoverRequestHeaders(request), plan.Body, "", previewContext)
		apiKey := strings.TrimSpace(strings.TrimPrefix(request.GetHeader("Authorization"), "Bearer "))
		if err := fingerprint.ClaimSessionIdentity(previewContext, candidate, apiKey); err != nil || fingerprint.accountIdentity == nil {
			trace.RejectAccount(candidate.ID(), "outbound_identity_unavailable")
			handler.store.Release(candidate)
			continue
		}
		if !handler.store.AdmitAccountSession(candidate, key, time.Now(), trace) {
			trace.RejectAccount(candidate.ID(), "session_capacity_exhausted")
			handler.store.Release(candidate)
			continue
		}
		ownerChanged := old != nil && !old.HasExactGroupIDs(ownerGroups)
		if old == nil {
			change := handler.missingSessionOwnerChange(ctx, oldID, plan.MissingOwner)
			ownerChanged = change != ""
			if ownerChanged {
				plan.Diagnostic.OwnerLookup = "recheck_" + change
			}
		}
		if ownerChanged || groupsKnown && !candidate.HasExactGroupIDs(ownerGroups) {
			if candidate.ID() != oldID {
				handler.store.RemoveAccountSession(candidate.ID(), key)
			}
			handler.store.Release(candidate)
			plan.Diagnostic.Result, plan.Diagnostic.Reason = "blocked", "account_groups_changed"
			if old == nil {
				plan.Diagnostic.Reason = "missing_owner_changed"
			}
			return nil, "", true
		}
		input := database.SessionAccountFailover{RootKey: state.Key, AffinityKey: key, ExpectedAccountID: oldID, AccountID: candidate.ID(), ExpectedGeneration: entry.Record.FailoverCount, UpstreamMode: targetMode, Reason: plan.Diagnostic.Reason, At: time.Now().UTC(), ResetOutboundWindow: true, WindowThreadID: state.ThreadID, WindowNumber: state.Number}
		input.WindowContextID = fingerprint.accountWindowInputs[state.ThreadID].ContextID
		if !state.Known {
			input.DeferOutboundWindow = true
			input.WindowThreadID, input.WindowNumber, input.WindowContextID = "", 0, ""
		}
		input.LossyContextRestart = true
		input.PreserveRestartInput = plan.PreserveInput
		grant := windowGrantForRequest(request)
		if grant != nil {
			input.WindowSubject = cache.PromptSessionLimitSubject(grant.Platform, grant.UserID)
			input.WindowRoot, input.WindowGrantID = grant.Grant.Root, grant.Grant.ID
			input.AllowPendingWindowGrant = !grant.Grant.Confirmed
		}
		committed, updatedGrant, commitErr := handler.db.SwitchSessionContinuityAccount(ctx, input)
		if commitErr != nil {
			if candidate.ID() != oldID {
				handler.store.RemoveAccountSession(candidate.ID(), key)
			}
			handler.store.Release(candidate)
			plan.Diagnostic.Result, plan.Diagnostic.Reason = "blocked", "ownership_commit_failed"
			return nil, "", true
		}
		if candidate.ID() != oldID {
			handler.store.UnbindSessionAffinity(key, oldID)
		}
		handler.store.BindSessionAffinity(key, candidate, candidate.GetProxyURL())
		handler.cacheSessionContinuity(state.Key, sessionContinuityCacheEntry{Record: committed, CheckedAt: time.Now(), WrittenAt: time.Now()})
		state.Record = committed
		state.RestartReason = ""
		handler.attachSessionOutboundEpoch(request, state.Key, committed)
		state.Diagnostic.OwnerAccount, state.Diagnostic.OwnerSource = candidate.ID(), "account_failover"
		selectionTraceForRequest(request).PinAccount(candidate.ID())
		plan.Diagnostic.Result, plan.Diagnostic.AccountID, plan.Diagnostic.Generation = "switched", candidate.ID(), committed.FailoverCount
		plan.Diagnostic.Phase = "after_switch"
		plan.Diagnostic.UpstreamMode = committed.UpstreamMode
		recordUsageRootAccount(request, candidate.ID(), true)
		if updatedGrant != nil && grant != nil {
			grant.Grant = *updatedGrant
			handler.cacheWindowTariff(input.WindowSubject, *updatedGrant)
			handler.publishRequestWindowGrant(request, grant)
		}
		return candidate, candidate.GetProxyURL(), true
	}
	plan.Diagnostic.Result = "no_safe_candidate"
	return nil, "", true
}
