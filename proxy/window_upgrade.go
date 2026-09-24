package proxy

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func (handler *Handler) cacheWindowTariff(subject string, grant database.UserWindowGrant) {
	handler.windowTariffMu.Lock()
	defer handler.windowTariffMu.Unlock()
	if handler.windowTariffs == nil {
		handler.windowTariffs = make(map[string]database.UserWindowGrant)
	}
	key := subject + ":" + grant.Root
	if current, exists := handler.windowTariffs[key]; exists && current.CreatedAt.Equal(grant.CreatedAt) && current.UpgradedAt != nil && grant.UpgradedAt == nil {
		return
	}
	if len(handler.windowTariffs) >= 32768 {
		if _, exists := handler.windowTariffs[key]; !exists {
			for candidate := range handler.windowTariffs {
				delete(handler.windowTariffs, candidate)
				break
			}
		}
	}
	handler.windowTariffs[key] = grant
}

func (handler *Handler) currentWindowTariff(ctx context.Context, subject, root string) (database.UserWindowGrant, bool, error) {
	handler.windowTariffMu.Lock()
	grant, found := handler.windowTariffs[subject+":"+root]
	handler.windowTariffMu.Unlock()
	if handler.db == nil && found && grant.ExpiresAt.After(time.Now()) {
		return grant, true, nil
	}
	if handler.db == nil {
		return grant, false, nil
	}
	lookup, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	state, err := handler.db.ReadUserWindowAdmissions(lookup, subject)
	if err != nil {
		return grant, false, err
	}
	if current := state.Windows[root]; current != nil {
		handler.cacheWindowTariff(subject, *current)
		return *current, true, nil
	}
	return grant, false, nil
}

func (handler *Handler) bindWindowGrantOwner(request *gin.Context, accountID int64, key string) error {
	grant := windowGrantForRequest(request)
	if grant == nil || accountID <= 0 || key == "" {
		return nil
	}
	if grant.Grant.OwnerAccountID > 0 {
		if grant.Grant.OwnerAccountID != accountID {
			return errors.New("窗口已绑定其他账号")
		}
		return nil
	}
	if handler.db == nil {
		return nil
	}
	subject := cache.PromptSessionLimitSubject(grant.Platform, grant.UserID)
	ctx, stop := context.WithTimeout(request.Request.Context(), time.Second)
	defer stop()
	var current database.UserWindowGrant
	err := handler.db.UpdateUserWindowAdmissions(ctx, subject, func(state *database.UserWindowAdmissionState) error {
		stored := state.Windows[grant.Grant.Root]
		if stored == nil || stored.ID != grant.Grant.ID || !stored.ExpiresAt.After(time.Now()) {
			return errWindowGrantRefresh
		}
		if stored.OwnerAccountID > 0 && stored.OwnerAccountID != accountID {
			return errors.New("窗口已绑定其他账号")
		}
		stored.OwnerAccountID, stored.OwnerKey = accountID, key
		current = *stored
		return nil
	})
	if err != nil {
		return err
	}
	grant.Grant = current
	handler.cacheWindowTariff(subject, current)
	return nil
}

func (handler *Handler) windowQuoteOwner(request *gin.Context, identity verifiedNewAPIPolicyContext) (int64, string, error) {
	diagnostic := windowControlDiagnostic(request)
	if diagnostic != nil {
		diagnostic.OwnerSource = "none"
	}
	key := sessionAffinityKey("newapi-root-session:"+identity.Meta.RootSessionFingerprint, identity.APIKeyID)
	lookup := appendWindowOwnerLookup(diagnostic, "current_root", identity.Meta.RootSessionFingerprint, key)
	entry, found, err := handler.readSessionContinuity(request.Request.Context(), hashRiskIdentity(key))
	if err != nil {
		lookup.Persistent, lookup.Result, lookup.ErrorKind = "failed", "lookup_failed", windowLookupErrorKind(err)
		return 0, "", err
	}
	if found {
		lookup.Persistent, lookup.Result, lookup.AccountID = "found", "found", entry.Record.AccountID
		if diagnostic != nil {
			diagnostic.OwnerSource = "continuity"
			diagnostic.OwnerLastSeen, diagnostic.OwnerLastCompleted, diagnostic.OwnerLastStatus = entry.Record.LastSeen, entry.Record.LastCompleted, entry.Record.LastStatus
		}
		return entry.Record.AccountID, key, nil
	}
	lookup.Persistent = "missing"
	if owner, found := handler.store.LiveSessionAccountID(key, time.Now()); found {
		lookup.Live, lookup.Result, lookup.AccountID = "found", "found", owner
		if diagnostic != nil {
			diagnostic.OwnerSource = "live_session"
		}
		return owner, key, nil
	}
	lookup.Live, lookup.Result = "missing", "missing"
	if userForkWindow(identity.Meta) {
		if diagnostic != nil {
			diagnostic.OwnerSource = "fork_parent"
		}
		parentKey := sessionAffinityKey("newapi-root-session:"+identity.Meta.ForkedFromSessionFingerprint, identity.APIKeyID)
		parentLookup := appendWindowOwnerLookup(diagnostic, "fork_parent", identity.Meta.ForkedFromSessionFingerprint, parentKey)
		owner, _, err := handler.resolveForkSourceOwnerWithDiagnostic(request.Request.Context(), requestSessionIdentity{forkSourceAffinityID: "newapi-root-session:" + identity.Meta.ForkedFromSessionFingerprint}, key, identity.APIKeyID, parentLookup)
		if err == nil && CurrentRuntimeSettings().CodexForkAccountFallbackEnabled {
			handler.deferForkQuoteOwner(request, owner, key)
			return 0, key, nil
		}
		if err != nil || owner == 0 {
			return 0, "", errors.New("无法恢复 fork 父会话账号")
		}
		return owner, key, nil
	}
	return 0, "", nil
}

func appendWindowOwnerLookup(diagnostic *database.WindowControlDiagnostic, target, fingerprint, key string) *database.WindowOwnerLookupDiagnostic {
	lookup := database.WindowOwnerLookupDiagnostic{Target: target, IdentitySource: "signed_original_root_fingerprint", RootHash: hashRiskIdentity(fingerprint), ScopeHash: hashRiskIdentity(key), Persistent: "not_checked", Live: "not_checked", Result: "not_checked"}
	if target == "fork_parent" {
		lookup.IdentitySource = "signed_original_fork_parent_fingerprint"
	}
	if diagnostic == nil {
		return &lookup
	}
	diagnostic.OwnerLookups = append(diagnostic.OwnerLookups, lookup)
	return &diagnostic.OwnerLookups[len(diagnostic.OwnerLookups)-1]
}

func windowLookupErrorKind(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "storage_error"
	}
}

func userForkWindow(meta newAPIPolicyMeta) bool {
	return meta.ThreadSource == "user" && meta.RequestKind == "turn" && meta.SubagentKind == "" && meta.PassiveFeature == "" && meta.SessionAccounting != newAPISessionAccountingBypass && meta.ForkedFromSessionFingerprint != "" && meta.ForkedFromSessionFingerprint != meta.RootSessionFingerprint
}

func (handler *Handler) upgradePersonalWindow(request *gin.Context, identity verifiedNewAPIPolicyContext, input windowControlRequest, windows map[string]personalWindow) {
	if !input.AllowExpansion || input.Multiplier <= 1 || input.ExtraLimit <= 0 || len(input.Root) > 64 || len(input.GrantID) > 64 || strings.TrimSpace(input.GrantID) == "" {
		writeWindowControlError(request, http.StatusBadRequest, "window_upgrade_confirmation_required", "请先开启扩容并单独确认此窗口的新倍率")
		return
	}
	subject := cache.PromptSessionLimitSubject(identity.Platform, identity.Identity.UserID)
	ctx, stop := context.WithTimeout(request.Request.Context(), 2*time.Second)
	defer stop()
	state, err := handler.db.ReadUserWindowAdmissions(ctx, subject)
	if err != nil {
		writeWindowControlError(request, http.StatusServiceUnavailable, "window_storage_unavailable", errWindowGrantStorage.Error())
		return
	}
	grant := state.Windows[input.Root]
	if grant == nil || grant.ID != input.GrantID || !grant.Confirmed || !grant.ExpiresAt.After(time.Now()) || grant.OwnerAccountID <= 0 || grant.OwnerKey == "" || grant.NoWindow {
		writeWindowControlError(request, http.StatusBadRequest, "window_upgrade_stale", "窗口状态已变化或尚无可恢复的账号归属，请刷新后重试")
		return
	}
	if grant.Expanded {
		request.JSON(http.StatusOK, gin.H{"version": 1})
		return
	}
	var upgraded database.UserWindowGrant
	err = handler.store.WithExpandedSessionReservation(grant.OwnerAccountID, grant.OwnerKey, func() error {
		return handler.db.UpdateUserWindowAdmissions(ctx, subject, func(state *database.UserWindowAdmissionState) error {
			current := state.Windows[input.Root]
			now := time.Now().UTC()
			if current == nil || current.ID != input.GrantID || !current.Confirmed || !current.ExpiresAt.After(now) || current.OwnerAccountID != grant.OwnerAccountID || current.OwnerKey != grant.OwnerKey || current.Expanded {
				return errors.New("窗口状态已变化，请刷新并重新确认")
			}
			counted := make(map[string]bool)
			for root, window := range windows {
				if window.Expanded && window.ExpiresAt.After(now) {
					counted[root] = true
				}
			}
			for root, window := range state.Windows {
				if window != nil && window.Expanded && window.ExpiresAt.After(now) && (window.Confirmed || window.PendingUntil.After(now)) {
					counted[root] = true
				}
			}
			if len(counted) >= input.ExtraLimit {
				return errors.New("你的扩容窗口额度已用尽，请等待恢复或使用已有窗口")
			}
			current.ID = uuid.NewString()
			current.Expanded, current.Multiplier, current.ExtraLimit, current.UpgradedAt = true, input.expansionMultiplier(len(counted)+1), input.ExtraLimit, &now
			upgraded = *current
			return nil
		})
	})
	if err != nil {
		writeWindowControlError(request, http.StatusBadRequest, "window_upgrade_rejected", err.Error())
		return
	}
	handler.cacheWindowTariff(subject, upgraded)
	handler.promptSessionLimitMu.Lock()
	if details := handler.promptSessionWindowDetails[subject]; details != nil {
		detail := details[input.Root]
		detail.GrantID, detail.Expanded, detail.Multiplier = upgraded.ID, true, upgraded.Multiplier
		details[input.Root] = detail
	}
	handler.promptSessionLimitMu.Unlock()
	handler.persistPromptSessionLimits(subject, time.Now())
	request.JSON(http.StatusOK, gin.H{"version": 1})
}
