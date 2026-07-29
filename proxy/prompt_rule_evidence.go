package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

const promptRuleLearningEvidenceContextKey = "prompt_rule_learning_evidence"

type promptRuleLearningEvidence struct {
	Text       string
	Endpoint   string
	Protocol   string
	Provider   string
	Model      string
	Action     string
	Score      int
	AuditScore int
	ReasonCode string
	Matches    []string
}

// capturePromptRuleLearningEvidence retains only the already-extracted user
// review text for the lifetime of the request. It performs no database I/O and
// no fingerprinting on the normal request path; those happen only if the
// upstream actually returns a cyber_policy result.
func (h *Handler) capturePromptRuleLearningEvidence(c *gin.Context, endpoint, model string, evaluation promptGuardEvaluation) {
	if c == nil {
		return
	}
	text := strings.TrimSpace(promptGuardReviewText(evaluation.Decision, evaluation.Envelope))
	if text == "" {
		return
	}
	matches := make([]string, 0, len(evaluation.Verdict.Matched))
	for _, match := range evaluation.Verdict.Matched {
		if name := strings.TrimSpace(match.Name); name != "" {
			matches = append(matches, name)
		}
	}
	protocol := ""
	if evaluation.Envelope.Protocol != promptfilter.ProtocolUnknown {
		protocol = string(evaluation.Envelope.Protocol)
	}
	provider := ""
	if evaluation.Envelope.ModelFamily != promptfilter.ModelFamilyUnknown {
		provider = string(evaluation.Envelope.ModelFamily)
	}
	c.Set(promptRuleLearningEvidenceContextKey, promptRuleLearningEvidence{
		Text: text, Endpoint: endpoint, Protocol: protocol, Provider: provider, Model: model,
		Action: evaluation.Verdict.Action, Score: evaluation.Verdict.Score, AuditScore: evaluation.Decision.AuditScore,
		ReasonCode: evaluation.Decision.ReasonCode, Matches: matches,
	})
}

func (h *Handler) enqueueUpstreamCyberPolicyEvidence(c *gin.Context, endpoint, model, errorCode string) {
	if h == nil || h.db == nil || c == nil {
		return
	}
	raw, exists := c.Get(promptRuleLearningEvidenceContextKey)
	captured, ok := raw.(promptRuleLearningEvidence)
	if !exists || !ok || strings.TrimSpace(captured.Text) == "" {
		captured, ok = h.capturePromptRuleLearningEvidenceOnUpstreamFailure(c, endpoint, model)
		if !ok {
			return
		}
	}
	fingerprint := promptfilter.PromptEvidenceFingerprint(captured.Text)
	if fingerprint == "" {
		return
	}
	audit := h.capturePromptFilterAuditContext(c)
	platform := ""
	if raw, exists := c.Get(newAPIPolicyMetaContextKey); exists {
		if policyContext, ok := raw.(verifiedNewAPIPolicyContext); ok {
			platform = policyContext.Platform
		}
	}
	if platform == "" {
		if raw, exists := c.Get(newAPIIdentityContextKey); exists {
			if identityContext, ok := raw.(verifiedNewAPIIdentityContext); ok {
				platform = identityContext.Platform
			}
		}
	}
	requestID := strings.TrimSpace(c.GetHeader("X-NewAPI-Request-ID"))
	if requestID == "" {
		requestID = strings.TrimSpace(c.GetHeader("X-Request-ID"))
	}
	if requestID == "" {
		requestID = fmt.Sprintf("cyber-%s-%d", fingerprint[:16], time.Now().UnixNano())
	}
	if endpoint == "" {
		endpoint = captured.Endpoint
	}
	if model == "" {
		model = captured.Model
	}
	protocol := captured.Protocol
	if audit.Protocol != "" {
		protocol = audit.Protocol
	}
	provider := captured.Provider
	if audit.Provider != "" {
		provider = audit.Provider
	}
	metadata, _ := json.Marshal(map[string]any{
		"error_code": errorCode, "endpoint": endpoint, "local_action": captured.Action,
		"local_score": captured.Score, "local_audit_score": captured.AuditScore,
		"local_reason_code": captured.ReasonCode, "local_matches": captured.Matches, "platform": platform,
	})
	preview := promptfilter.RedactedPreview(captured.Text, 2000)
	candidate := database.PromptRuleCandidateInput{
		Fingerprint: fingerprint, Kind: database.PromptRuleCandidateKindEvidence,
		Source: database.PromptRuleCandidateSourceUpstreamCyberPolicy, SamplePreview: preview,
		Rationale: "上游返回 cyber_policy，等待归因和候选规则审核",
	}
	evidence := database.PromptRuleCandidateEvidenceInput{
		SourceKind: database.PromptRuleCandidateSourceUpstreamCyberPolicy,
		SourceRef:  requestID,
		SourceRefHash: promptfilter.StableEvidenceFingerprint(
			"cyber-event", fmt.Sprintf("%d\x00%s\x00%s", audit.APIKeyID, platform, requestID),
		),
		SamplePreview: preview, MetadataJSON: string(metadata), Protocol: protocol, Provider: provider, Model: model,
		APIKeyID: audit.APIKeyID, APIKeyName: audit.APIKeyName, ObservedAt: time.Now().UTC(),
	}
	h.db.EnqueuePromptRuleCandidate(&candidate, &evidence, database.PromptFilterLogPriorityHigh)
}

// capturePromptRuleLearningEvidenceOnUpstreamFailure is a rare-path fallback
// for deployments that disabled local filtering. It parses the request only
// after an upstream cyber_policy result, preserving the normal fast path while
// still retaining a global evidence candidate for later human attribution.
func (h *Handler) capturePromptRuleLearningEvidenceOnUpstreamFailure(c *gin.Context, endpoint, model string) (promptRuleLearningEvidence, bool) {
	if h == nil || h.store == nil || c == nil {
		return promptRuleLearningEvidence{}, false
	}
	raw, exists := c.Get("raw_body")
	body, ok := raw.([]byte)
	if !exists || !ok || len(body) == 0 {
		return promptRuleLearningEvidence{}, false
	}
	cfg := h.promptFilterConfigForRequest(c)
	envelope := promptfilter.BuildEnvelopeWithModelsAndPerformance(
		body, endpoint, model, model, promptfilter.TransportHTTP, cfg.MaxTextLength, cfg.Advanced.Guard.Performance,
	)
	text := strings.TrimSpace(envelopeCurrentUserText(envelope))
	if text == "" {
		return promptRuleLearningEvidence{}, false
	}
	protocol := ""
	if envelope.Protocol != promptfilter.ProtocolUnknown {
		protocol = string(envelope.Protocol)
	}
	provider := ""
	if envelope.ModelFamily != promptfilter.ModelFamilyUnknown {
		provider = string(envelope.ModelFamily)
	}
	return promptRuleLearningEvidence{
		Text: text, Endpoint: endpoint, Protocol: protocol, Provider: provider, Model: model,
		Action: promptfilter.ActionAllow,
	}, true
}
