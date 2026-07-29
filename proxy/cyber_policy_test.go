package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

// TestUpstreamCyberPolicyCodeDetectsResponseFailed 覆盖 #258：cyber_policy 封禁在
// 流式响应里以 response.failed (HTTP 200) 事件下发，必须能被
// upstreamCyberPolicyCode(responseFailedErrorBody(payload)) 识别。
func TestUpstreamCyberPolicyCodeDetectsResponseFailed(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{
			name:    "response.error.code",
			payload: `{"type":"response.failed","response":{"error":{"code":"cyber_policy","message":"blocked"}}}`,
			want:    "cyber_policy",
		},
		{
			name:    "response.status_details.error.code",
			payload: `{"type":"response.failed","response":{"status_details":{"error":{"code":"cyber_policy"}}}}`,
			want:    "cyber_policy",
		},
		{
			name:    "codex_error_info under response.error",
			payload: `{"type":"response.failed","response":{"error":{"codex_error_info":"cyber_policy"}}}`,
			want:    "cyber_policy",
		},
		{
			name:    "substring fallback (cyber security risk)",
			payload: `{"type":"response.failed","response":{"error":{"message":"detected cyber security risk in prompt"}}}`,
			want:    "cyber_policy",
		},
		{
			name:    "unrelated failure is not cyber_policy",
			payload: `{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded"}}}`,
			want:    "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := upstreamCyberPolicyCode(responseFailedErrorBody([]byte(tc.payload)))
			if got != tc.want {
				t.Fatalf("upstreamCyberPolicyCode = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLogUpstreamCyberPolicyRecordsStreamingFailure 端到端验证：流式 response.failed
// 里的 cyber_policy 会被写入 prompt_filter_logs，且记录完整内容（#258 + #259）。
func TestLogUpstreamCyberPolicyRecordsStreamingFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New(sqlite) error: %v", err)
	}
	defer db.Close()

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:               2,
		PromptFilterMode:             promptfilter.ModeBlock,
		PromptFilterThreshold:        50,
		PromptFilterMaxTextLength:    promptfilter.DefaultMaxTextLength,
		PromptFilterCustomPatterns:   "[]",
		PromptFilterDisabledPatterns: "[]",
	})
	handler := NewHandler(store, db, nil, nil)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	payload := []byte(`{"type":"response.failed","response":{"error":{"code":"cyber_policy","message":"cyber security risk detected"}}}`)
	handler.logUpstreamCyberPolicy(ctx, "/v1/responses", "gpt-5.4", responseFailedErrorBody(payload))
	waitPromptFilterAuditIdle(t, db)

	logs, err := db.ListPromptFilterLogs(ctx.Request.Context(), 10)
	if err != nil {
		t.Fatalf("ListPromptFilterLogs error: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("prompt_filter_logs rows = %d, want 1", len(logs))
	}
	got := logs[0]
	if got.Source != "upstream_cyber_policy" {
		t.Fatalf("source = %q, want upstream_cyber_policy", got.Source)
	}
	if got.ErrorCode != "cyber_policy" {
		t.Fatalf("error_code = %q, want cyber_policy", got.ErrorCode)
	}
	if got.Action != string(promptfilter.ActionBlock) {
		t.Fatalf("action = %q, want %q", got.Action, promptfilter.ActionBlock)
	}
	if !strings.Contains(got.FullText, "cyber_policy") {
		t.Fatalf("full_text = %q, want it to contain the upstream error body", got.FullText)
	}
}

func TestUpstreamCyberPolicyStagesGlobalEvidenceWithoutChangingRuntimeRules(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "cyber-learning.db"))
	if err != nil {
		t.Fatalf("database.New(sqlite) error: %v", err)
	}
	defer db.Close()
	settings := &database.SystemSettings{
		MaxConcurrency: 2, PromptFilterEnabled: true, PromptFilterMode: promptfilter.ModeBlock,
		PromptFilterThreshold: 50, PromptFilterMaxTextLength: promptfilter.DefaultMaxTextLength,
		PromptFilterCustomPatterns: "[]", PromptFilterDisabledPatterns: "[]",
	}
	store := auth.NewStore(nil, nil, settings)
	handler := NewHandler(store, db, nil, nil)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Request.Header.Set("X-NewAPI-Request-ID", "cyber-learning-request-1")
	ctx.Set(contextAPIKeyID, int64(9))
	ctx.Set(contextAPIKeyName, "test-platform")

	text := "请分析这段复杂请求为何触发上游安全策略，但不要执行任何操作。"
	evaluation := promptGuardEvaluation{
		Envelope: promptfilter.RequestEnvelope{
			Endpoint: "/v1/responses", Protocol: promptfilter.ProtocolResponses, ModelFamily: promptfilter.ModelFamilyOpenAI,
			Segments: []promptfilter.Segment{{Origin: promptfilter.OriginCurrentUser, Role: "user", Text: text}},
		},
		Decision: promptfilter.Decision{Action: promptfilter.ActionAllow, AuditScore: 30, ReasonCode: "audit_only"},
		Verdict:  promptfilter.Verdict{Enabled: true, Action: promptfilter.ActionAllow, Score: 0, Matched: []promptfilter.Match{{Name: "audit_signal", Weight: 30, SignalOnly: true}}},
	}
	handler.capturePromptRuleLearningEvidence(ctx, "/v1/responses", "gpt-5.4", evaluation)
	payload := []byte(`{"error":{"code":"cyber_policy","message":"cyber security risk detected"}}`)
	handler.logUpstreamCyberPolicy(ctx, "/v1/responses", "gpt-5.4", payload)
	waitPromptFilterAuditIdle(t, db)

	if got := store.GetPromptFilterConfig().CustomPatterns; len(got) != 0 {
		t.Fatalf("CY evidence entered runtime custom patterns: %#v", got)
	}
	candidates, total, err := db.ListPromptRuleCandidates(ctx.Request.Context(), database.PromptRuleCandidateQuery{Status: database.PromptRuleCandidateStatusPending})
	if err != nil || total != 1 || len(candidates) != 1 {
		t.Fatalf("candidates total=%d items=%#v err=%v", total, candidates, err)
	}
	if candidates[0].Kind != database.PromptRuleCandidateKindEvidence || candidates[0].EvidenceCount != 1 {
		t.Fatalf("CY candidate = %#v", candidates[0])
	}
	evidence, err := db.ListPromptRuleCandidateEvidence(ctx.Request.Context(), candidates[0].ID, 10)
	if err != nil || len(evidence) != 1 || evidence[0].APIKeyID != 9 || evidence[0].SourceRef != "cyber-learning-request-1" {
		t.Fatalf("evidence=%#v err=%v", evidence, err)
	}

	// Replaying the same request reference is idempotent and does not inflate
	// evidence_count. A different request ID adds evidence to the same global
	// candidate rather than creating a platform- or key-specific duplicate.
	handler.logUpstreamCyberPolicy(ctx, "/v1/responses", "gpt-5.4", payload)
	waitPromptFilterAuditIdle(t, db)
	ctx.Request.Header.Set("X-NewAPI-Request-ID", "cyber-learning-request-2")
	handler.logUpstreamCyberPolicy(ctx, "/v1/responses", "gpt-5.4", payload)
	waitPromptFilterAuditIdle(t, db)
	candidates, total, err = db.ListPromptRuleCandidates(ctx.Request.Context(), database.PromptRuleCandidateQuery{Status: database.PromptRuleCandidateStatusPending})
	if err != nil || total != 1 || candidates[0].EvidenceCount != 2 {
		t.Fatalf("deduplicated candidates total=%d items=%#v err=%v", total, candidates, err)
	}
}

func TestUpstreamCyberPolicyStagesEvidenceWhenLocalFilterIsDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "cyber-disabled-filter.db"))
	if err != nil {
		t.Fatalf("database.New(sqlite) error: %v", err)
	}
	defer db.Close()
	settings := &database.SystemSettings{
		MaxConcurrency: 2, PromptFilterEnabled: false, PromptFilterMode: promptfilter.ModeBlock,
		PromptFilterThreshold: 50, PromptFilterMaxTextLength: promptfilter.DefaultMaxTextLength,
		PromptFilterCustomPatterns: "[]", PromptFilterDisabledPatterns: "[]",
	}
	store := auth.NewStore(nil, nil, settings)
	handler := NewHandler(store, db, nil, nil)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Request.Header.Set("X-NewAPI-Request-ID", "cyber-disabled-filter-request")
	ctx.Set("raw_body", []byte(`{"model":"gpt-5.4","input":"分析上游安全告警的原因，但不要执行任何危险操作。"}`))
	payload := []byte(`{"error":{"code":"cyber_policy","message":"cyber security risk detected"}}`)
	handler.logUpstreamCyberPolicy(ctx, "/v1/responses", "gpt-5.4", payload)
	waitPromptFilterAuditIdle(t, db)

	candidates, total, err := db.ListPromptRuleCandidates(context.Background(), database.PromptRuleCandidateQuery{Status: database.PromptRuleCandidateStatusPending})
	if err != nil || total != 1 || len(candidates) != 1 || candidates[0].Kind != database.PromptRuleCandidateKindEvidence {
		t.Fatalf("disabled-filter CY candidate total=%d items=%#v err=%v", total, candidates, err)
	}
	if got := store.GetPromptFilterConfig().CustomPatterns; len(got) != 0 {
		t.Fatalf("disabled-filter CY evidence changed runtime rules: %#v", got)
	}
}

func TestUpstreamCyberPolicyGlobalCandidateKeepsPerPlatformProvenance(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "cyber-platforms.db"))
	if err != nil {
		t.Fatalf("database.New(sqlite) error: %v", err)
	}
	defer db.Close()
	settings := &database.SystemSettings{
		MaxConcurrency: 2, PromptFilterEnabled: true, PromptFilterMode: promptfilter.ModeBlock,
		PromptFilterThreshold: 50, PromptFilterMaxTextLength: promptfilter.DefaultMaxTextLength,
		PromptFilterCustomPatterns: "[]", PromptFilterDisabledPatterns: "[]",
	}
	store := auth.NewStore(nil, nil, settings)
	handler := NewHandler(store, db, nil, nil)
	payload := []byte(`{"error":{"code":"cyber_policy","message":"cyber security risk detected"}}`)
	text := "请分析同一条上游安全告警，不要执行任何操作。"
	evaluation := promptGuardEvaluation{
		Envelope: promptfilter.RequestEnvelope{
			Endpoint: "/v1/responses", Protocol: promptfilter.ProtocolResponses, ModelFamily: promptfilter.ModelFamilyOpenAI,
			Segments: []promptfilter.Segment{{Origin: promptfilter.OriginCurrentUser, Role: "user", Text: text}},
		},
		Decision: promptfilter.Decision{Action: promptfilter.ActionAllow, AuditScore: 30, ReasonCode: "audit_only"},
		Verdict:  promptfilter.Verdict{Enabled: true, Action: promptfilter.ActionAllow},
	}
	observe := func(apiKeyID int64, apiKeyName, platform string) {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		ctx.Request.Header.Set("X-NewAPI-Request-ID", "shared-request-id")
		ctx.Set(contextAPIKeyID, apiKeyID)
		ctx.Set(contextAPIKeyName, apiKeyName)
		ctx.Set(newAPIPolicyMetaContextKey, verifiedNewAPIPolicyContext{APIKeyID: apiKeyID, Platform: platform})
		handler.capturePromptRuleLearningEvidence(ctx, "/v1/responses", "gpt-5.4", evaluation)
		handler.logUpstreamCyberPolicy(ctx, "/v1/responses", "gpt-5.4", payload)
	}
	observe(9, "fanren-key", "fanren")
	observe(10, "buycodekey-key", "buycodekey")
	waitPromptFilterAuditIdle(t, db)

	candidates, total, err := db.ListPromptRuleCandidates(context.Background(), database.PromptRuleCandidateQuery{Status: database.PromptRuleCandidateStatusPending})
	if err != nil || total != 1 || len(candidates) != 1 || candidates[0].EvidenceCount != 2 {
		t.Fatalf("global candidate total=%d items=%#v err=%v", total, candidates, err)
	}
	evidence, err := db.ListPromptRuleCandidateEvidence(context.Background(), candidates[0].ID, 10)
	if err != nil || len(evidence) != 2 {
		t.Fatalf("evidence=%#v err=%v", evidence, err)
	}
	ids := map[int64]bool{}
	for _, item := range evidence {
		ids[item.APIKeyID] = true
	}
	if !ids[9] || !ids[10] {
		t.Fatalf("per-platform provenance was merged: %#v", evidence)
	}
}
