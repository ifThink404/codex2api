package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

func newTurnStateTestContext(t *testing.T) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req, err := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest: %v", err)
	}
	c.Request = req
	return c, recorder
}

// 上游无 turn-state 时必须清除 writer 上残留的上一 failover attempt 的值,
// 防止旧账号的 blob 粘到新账号的响应上。
func TestRelayCodexTurnStateClearsStaleHeaderOnFailover(t *testing.T) {
	// 中继语义本身（真实 token 透传 + 换号后清除）；托管开启时下发的是替身，
	// 由 TestRelayCodexTurnStateResponseHeaderEmitsSubstitute 覆盖。
	disableTurnStateVault(t)
	affinityKey := "turn-guard-conv-stale::api-key:9"
	t.Cleanup(func() { codexTurnStateOrigins.Delete(affinityKey) })

	c, recorder := newTurnStateTestContext(t)
	first := http.Header{}
	first.Set(codexTurnStateHeader, "blob-attempt-1")
	relayCodexTurnStateResponseHeader(c, affinityKey, &auth.Account{DBID: 101}, "gpt-test", first)
	if got := recorder.Header().Get(codexTurnStateHeader); got != "blob-attempt-1" {
		t.Fatalf("first relay header = %q", got)
	}

	relayCodexTurnStateResponseHeader(c, affinityKey, &auth.Account{DBID: 202}, "gpt-test", http.Header{})
	if got := recorder.Header().Get(codexTurnStateHeader); got != "" {
		t.Fatalf("stale turn-state survived failover relay: %q", got)
	}
}

func TestCommitResponsesStreamAttemptStagesWinningTurnStateBeforeHeadersCommit(t *testing.T) {
	disableTurnStateVault(t)
	affinityKey := "turn-guard-winning-header::api-key:9"
	t.Cleanup(func() { codexTurnStateOrigins.Delete(affinityKey) })
	c, recorder := newTurnStateTestContext(t)
	flusher, _ := c.Writer.(http.Flusher)
	attempt := newContinuousRetryStreamAttempt(true, c.Writer, flusher)
	t.Cleanup(func() { _ = attempt.Close() })
	if _, err := attempt.replay.Write([]byte("data: {\"type\":\"response.completed\"}\n\n")); err != nil {
		t.Fatalf("buffer attempt: %v", err)
	}
	upstream := http.Header{}
	upstream.Set(codexTurnStateHeader, "winning-turn-state")
	account := &auth.Account{DBID: 303}

	if err := (&Handler{}).commitResponsesStreamAttempt(c, attempt, affinityKey, account, "gpt-test", upstream); err != nil {
		t.Fatalf("commit response attempt: %v", err)
	}
	result := recorder.Result()
	if got := result.Header.Get(codexTurnStateHeader); got != "winning-turn-state" {
		t.Fatalf("committed response header = %q, want winning-turn-state", got)
	}
	raw, ok := codexTurnStateOrigins.Load(affinityKey)
	origin, valid := raw.(codexTurnStateOrigin)
	if !ok || !valid || origin.accountID != account.ID() {
		t.Fatalf("winning turn-state provenance = %#v, present=%v", raw, ok)
	}
}

func TestCommitResponsesStreamAttemptDoesNotForgeTurnStateAfterHeartbeat(t *testing.T) {
	affinityKey := "turn-guard-heartbeat-committed::api-key:9"
	t.Cleanup(func() { codexTurnStateOrigins.Delete(affinityKey) })
	c, recorder := newTurnStateTestContext(t)
	_, _ = c.Writer.WriteString(continuousRetryKeepaliveComment)
	c.Writer.Flush()
	flusher, _ := c.Writer.(http.Flusher)
	attempt := newContinuousRetryStreamAttempt(true, c.Writer, flusher)
	t.Cleanup(func() { _ = attempt.Close() })
	if _, err := attempt.replay.Write([]byte("data: {\"type\":\"response.completed\"}\n\n")); err != nil {
		t.Fatalf("buffer attempt: %v", err)
	}
	upstream := http.Header{}
	upstream.Set(codexTurnStateHeader, "late-turn-state")

	if err := (&Handler{}).commitResponsesStreamAttempt(c, attempt, affinityKey, &auth.Account{DBID: 404}, "gpt-test", upstream); err != nil {
		t.Fatalf("commit response attempt: %v", err)
	}
	result := recorder.Result()
	if got := result.Header.Get(codexTurnStateHeader); got != "" {
		t.Fatalf("late turn-state was forged into committed headers: %q", got)
	}
	if _, ok := codexTurnStateOrigins.Load(affinityKey); ok {
		t.Fatal("undelivered turn-state recorded provenance")
	}
}

// 会话级 beta-features:未声明时补默认(deviceCfg 优先于内置默认),
// 客户端显式声明的原样保留(即使不含 remote_compaction_v2)。
func TestApplyCodexRequestHeadersSessionLevelBetaFeatures(t *testing.T) {
	acc := &auth.Account{DBID: 42, AccountID: "acct-42"}

	plain, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest: %v", err)
	}
	applyCodexRequestHeaders(plain, acc, "token-123", "cache-key-1", "api-key-1", nil, http.Header{})
	if got := plain.Header.Get(codexBetaFeaturesHeader); got != defaultCodexBetaFeatures {
		t.Fatalf("undeclared beta features = %q, want %q", got, defaultCodexBetaFeatures)
	}

	device, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest: %v", err)
	}
	applyCodexRequestHeaders(device, acc, "token-123", "cache-key-1", "api-key-1", &DeviceProfileConfig{BetaFeatures: "multi_agent"}, http.Header{})
	if got := device.Header.Get(codexBetaFeaturesHeader); got != "multi_agent" {
		t.Fatalf("device-profile beta features = %q, want multi_agent", got)
	}

	declared, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest: %v", err)
	}
	applyCodexRequestHeaders(declared, acc, "token-123", "cache-key-1", "api-key-1", nil, http.Header{
		codexBetaFeaturesHeader: []string{"custom_feature"},
	})
	if got := declared.Header.Get(codexBetaFeaturesHeader); got != "custom_feature" {
		t.Fatalf("client-declared beta features rewritten: %q", got)
	}
}
