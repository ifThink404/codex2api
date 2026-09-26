package proxy

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
)

type failoverClaimFailures struct {
	CodexIdentityStore
	calls int
}

func (s *failoverClaimFailures) ClaimCodexIdentities(ctx context.Context, keys []string, owner string) error {
	s.calls++
	if s.calls <= 16 {
		return errors.New("candidate identity temporarily unavailable")
	}
	return s.CodexIdentityStore.ClaimCodexIdentities(ctx, keys, owner)
}

func TestSessionFailoverSearchesBeyondSixteenCandidates(t *testing.T) {
	h, owner, _, key := failoverTestSetup(t, true)
	owner.Status = auth.StatusError
	h.store.SetSchedulerEngine("legacy")
	for i := 0; i < 17; i++ {
		h.store.AddAccount(&auth.Account{DBID: int64(2000 + i), AccountID: fmt.Sprintf("861373c1-f1a9-4ca9-8682-%012d", i), AccessToken: "candidate", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}})
	}
	c, body := failoverTestRequest(t, h)
	require.Nil(t, h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
	claims := &failoverClaimFailures{CodexIdentityStore: h.db}
	ctx := WithCodexIdentityStore(c.Request.Context(), claims)
	selected, _, handled := h.takeSessionAccountFailover(ctx, key, 0, nil, nil, auth.DispatchPolicyStandard)
	require.True(t, handled)
	require.NotNil(t, selected)
	h.store.Release(selected)
	require.Equal(t, 17, claims.calls)
	require.Equal(t, 17, usageRequestDiagnosticState(c).AccountFailover.Selection.Attempts)
}

func TestSessionFailoverRechecksOwnerAfterSelectionFailure(t *testing.T) {
	h, owner, target, key := failoverTestSetup(t, true)
	c, body := failoverTestRequest(t, h)
	require.Nil(t, h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
	// A model update races the gap between request preparation and selection.
	owner.Models = []string{"gpt-5.6-terra"}
	exclusions := newSessionRetryAccountExclusions(c, key, body)
	selected, _, _ := h.nextRetryAccountForSessionWithDispatchGuard(c.Request.Context(), key, 0, exclusions, sessionModelSupportFilter("gpt-5.6-sol", "gpt-5.6-sol", false), auth.DispatchPolicyStandard)
	require.Same(t, target, selected)
	h.store.Release(selected)
	require.False(t, selectionTraceForRequest(c).SessionModelDenied())
}

func TestSessionReplayMessagesCanRestartAndRestore(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			h, owner, target, key := failoverTestSetup(t, true)
			owner.Models = []string{"gpt-5.6-terra"}
			c, body := failoverTestRequest(t, h)
			c.Request.URL.Path = path
			body, _ = sjson.DeleteBytes(body, "input")
			body, _ = sjson.SetRawBytes(body, "messages", []byte(`[{"role":"user","content":"continue the conversation"}]`))
			body, _ = sjson.SetBytes(body, "max_tokens", 32)
			before := string(body)
			require.Nil(t, h.prepareContinuityRestart(c, body))
			require.Nil(t, h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
			selected, _, handled := h.takeSessionAccountFailover(c.Request.Context(), key, 0, nil, nil, auth.DispatchPolicyStandard)
			require.True(t, handled)
			require.Same(t, target, selected)
			h.store.Release(selected)
			require.Nil(t, h.validateMigratedSessionContext(c, body, continuityRequest(c).Record, key))
			require.Equal(t, before, string(body), "migration validation must not mutate ingress")
		})
	}
}

func TestSessionFailoverUnavailableOwnershipIsRetryable(t *testing.T) {
	for _, reason := range []string{"ownership_changed", "ownership_unavailable"} {
		h, _, _, _ := failoverTestSetup(t, true)
		c, _ := failoverTestRequest(t, h)
		failure := sessionFailoverContextError(c, &sessionAccountFailoverDiagnostic{}, reason)
		require.Equal(t, 503, api.HTTPStatusCode(failure.Code))
		require.Equal(t, true, failure.Details.(gin.H)["retryable"])
		require.Equal(t, "true", c.Writer.Header().Get("X-Should-Retry"))
	}
}

// Every account-local rejection must reach the same migration transaction,
// whether the administrator enabled normal failover or relaxed mode.
func TestSessionFailoverOwnerRejectionMatrix(t *testing.T) {
	for _, relaxed := range []bool{false, true} {
		for _, scenario := range []string{"model", "route", "unauthorized", "payment", "error", "credential", "cooldown", "model_cooldown"} {
			name := scenario + map[bool]string{false: "_failover", true: "_relaxed"}[relaxed]
			t.Run(name, func(t *testing.T) {
				h, owner, target, key := failoverTestSetup(t, !relaxed)
				UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = relaxed; return s })
				switch scenario {
				case "model":
					owner.Models = []string{"gpt-5.6-terra"}
				case "route":
					off := false
					owner.CodexNative, owner.CodexBPS = &off, false
				case "unauthorized", "payment", "cooldown":
					reason := map[string]string{"unauthorized": "unauthorized", "payment": "payment_required", "cooldown": "server_error"}[scenario]
					owner.Status, owner.CooldownReason, owner.CooldownUtil = auth.StatusCooldown, reason, time.Now().Add(time.Hour)
				case "error":
					owner.Status = auth.StatusError
				case "credential":
					owner.AccessToken = ""
				case "model_cooldown":
					owner.ModelCooldowns = map[string]auth.ModelCooldown{"gpt-5.6-sol": {Model: "gpt-5.6-sol", Reason: "rate_limited", ResetAt: time.Now().Add(time.Hour)}}
				}
				c, body := failoverTestRequest(t, h)
				require.Nil(t, h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
				require.NotEmpty(t, h.forkAccountFallbackReason(c, owner.ID(), key+":fork"), "fork must recognize the same unavailable parent")
				selected, _, handled := h.takeSessionAccountFailover(c.Request.Context(), key, 0, nil, sessionModelSupportFilter("gpt-5.6-sol", "gpt-5.6-sol", false), auth.DispatchPolicyStandard)
				require.True(t, handled)
				require.Same(t, target, selected)
				h.store.Release(selected)
				record, found, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
				require.NoError(t, err)
				require.True(t, found)
				require.Equal(t, target.ID(), record.AccountID)
				require.EqualValues(t, 1, record.FailoverCount)
				require.False(t, selectionTraceForRequest(c).SessionModelDenied())
			})
		}
	}
}
