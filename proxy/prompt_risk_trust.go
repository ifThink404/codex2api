package proxy

import (
	"context"
	"sync"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

const promptRiskTrustBypassAuditInterval = 10 * time.Minute

var promptRiskTrustBypassAudit sync.Map // subject key -> time.Time

func (h *Handler) promptRiskTrustPolicyForRequest(c *gin.Context) (database.PromptRiskTrustPolicy, string, bool) {
	if h == nil || h.store == nil || c == nil {
		return database.PromptRiskTrustPolicy{}, "", false
	}
	raw, ok := c.Get(newAPIIdentityContextKey)
	if !ok {
		return database.PromptRiskTrustPolicy{}, "", false
	}
	identity, ok := raw.(verifiedNewAPIIdentityContext)
	if !ok {
		return database.PromptRiskTrustPolicy{}, "", false
	}
	subjectKey := database.PromptRiskNewAPIUserSubjectKey(identity.Platform, identity.Identity.UserID)
	policy, ok := h.store.GetPromptRiskTrustPolicy(subjectKey, time.Now().UTC())
	return policy, subjectKey, ok
}

func promptRiskTrustCanBypassReview(decision promptfilter.Decision, verdict promptfilter.Verdict, reviewText string) bool {
	if reviewText == "" || decision.Action != promptfilter.ActionAllow || verdict.Action != promptfilter.ActionAllow {
		return false
	}
	if decision.AuditScore > 0 || decision.AuditRawScore > 0 || len(decision.Signals) > 0 || len(verdict.Matched) > 0 {
		return false
	}
	return len(decision.Errors) == 0 && verdict.ReviewError == ""
}

func promptRiskTrustShouldSuspend(decision promptfilter.Decision, verdict promptfilter.Verdict) bool {
	return decision.Action != promptfilter.ActionAllow || verdict.Action != promptfilter.ActionAllow ||
		decision.AuditScore > 0 || decision.AuditRawScore > 0 || len(decision.Signals) > 0 ||
		len(verdict.Matched) > 0 || len(decision.Errors) > 0 || verdict.ReviewError != ""
}

func (h *Handler) recordPromptRiskTrustBypass(c *gin.Context, policy database.PromptRiskTrustPolicy, subjectKey string) {
	if h == nil || h.db == nil || policy.ID <= 0 || subjectKey == "" {
		return
	}
	now := time.Now().UTC()
	if raw, ok := promptRiskTrustBypassAudit.Load(subjectKey); ok {
		if last, valid := raw.(time.Time); valid && now.Sub(last) < promptRiskTrustBypassAuditInterval {
			return
		}
	}
	promptRiskTrustBypassAudit.Store(subjectKey, now)
	requestIDHash := promptfilter.StableEvidenceFingerprint("adaptive-trust-request", ensurePromptPolicyRequestCorrelationID(c))
	h.db.RunBackgroundTask(func(ctx context.Context) {
		_ = h.db.RecordPromptRiskTrustBypass(ctx, policy.ID, policy.SubjectType, subjectKey, requestIDHash)
	})
}

func (h *Handler) suspendPromptRiskTrustPolicy(policy database.PromptRiskTrustPolicy, subjectKey, reason string) {
	if h == nil || h.store == nil || subjectKey == "" {
		return
	}
	h.store.RemovePromptRiskTrustPolicy(subjectKey)
	if h.db == nil || policy.ID <= 0 {
		return
	}
	score := policy.LastRiskScore
	if score < policy.RiskThreshold {
		score = policy.RiskThreshold
	}
	h.db.RunBackgroundTask(func(ctx context.Context) {
		_, _ = h.db.SuspendPromptRiskTrustPolicy(ctx, policy.SubjectType, subjectKey, reason, score, database.PromptRiskLevelElevated)
	})
}
