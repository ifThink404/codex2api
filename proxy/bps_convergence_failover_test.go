package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestBPSConvergenceFailoverKeepsAccountTaskAndRotatesTurn(t *testing.T) {
	for _, mode := range []string{auth.CodexFingerprintModeRound, auth.CodexFingerprintModeTurnRound} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("CODEX_TRANSPORT_MODE", "standard")
			h, a, b, key := failoverTestSetup(t, true)
			off := false
			for _, account := range []*auth.Account{a, b} {
				account.CodexBPS, account.CodexNative, account.CodexFingerprintMode = true, &off, mode
				installClaudeBoundaryTransport(t, account, func(r *http.Request) (*http.Response, error) {
					wire, err := io.ReadAll(r.Body)
					require.NoError(t, err)
					d := r.Context().Value(codexBPSDiagnosticKey{}).(*CodexBPSDiagnostic).WordIdentity
					require.Equal(t, d.TaskID, gjson.GetBytes(wire, "metadata.task_id").String())
					require.Equal(t, d.TurnID, gjson.GetBytes(wire, "metadata.turn_id").String())
					require.Equal(t, d.AgentIteration, gjson.GetBytes(wire, "metadata.agent_iteration").String())
					require.Equal(t, account.EffectiveAccountID(), r.Header.Get("Chatgpt-Account-Id"))
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"output":[]}`)), Request: r}, nil
				})
			}
			key += "-bps"
			h.store.BindSessionAffinity(key, a, "")
			_, err := h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(key), database.SessionContinuityRecord{AccountID: a.ID(), UpstreamMode: "bps", ThreadID: continuityTestThread, NumberKnown: true})
			require.NoError(t, err)
			// B already has an active account/model/effort task before A switches.
			warmCtx, err := withBPSFullConvergence(WithCodexIdentityStore(t.Context(), h.db), b, bpsProfile(auth.BPSWord), nil, nil, "another-user", "another-key")
			require.NoError(t, err)
			warm, err := resolveBPSWordIdentity(warmCtx, []byte(`{"input":"prewarm"}`), nil, "another-user", "gpt-5.6-sol", false)
			require.NoError(t, err)
			input := func(n int) string {
				value := `[{"role":"user","content":"same user question"}`
				for i := 0; i < n; i++ {
					value += fmt.Sprintf(`,{"type":"function_call","name":"echo","call_id":"c%d","arguments":"{}"},{"type":"function_call_output","call_id":"c%d","output":"done"}`, i, i)
				}
				return value + "]"
			}
			send := func(account *auth.Account, tools int, migrate bool) *bpsWordIdentityDiagnostic {
				t.Helper()
				if migrate {
					atomic.StoreInt32(&a.Disabled, 1)
					atomic.StoreInt32(&b.Disabled, 1)
					atomic.StoreInt32(&account.Disabled, 0)
				}
				c, body := failoverTestRequest(t, h)
				body, err = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.turn_id", "01a0939f-d89c-77f1-94fa-000000000123")
				require.NoError(t, err)
				body, err = sjson.SetRawBytes(body, "input", []byte(input(tools)))
				require.NoError(t, err)
				c.Set(ingressRequestBodyContextKey, body)
				require.Nil(t, h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
				if migrate {
					selected, _, handled := h.takeSessionAccountFailover(c.Request.Context(), key, 0, nil, nil, auth.DispatchPolicyStandard)
					require.True(t, handled)
					require.Same(t, account, selected)
					h.store.Release(selected)
				}
				require.Nil(t, h.commitSessionContinuity(c, account))
				resp, err := ExecuteRequest(c.Request.Context(), account, body, "cache", "", "test-user-key", nil, c.Request.Header, false)
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
				return CodexBPSResponseDiagnostic(resp).WordIdentity
			}
			a1 := send(a, 0, false)
			a2 := send(a, 1, false)
			require.Equal(t, a1.TurnID, a2.TurnID)
			require.Equal(t, "2", a2.AgentIteration)
			b1 := send(b, 1, true)
			require.Equal(t, warm.TaskID, b1.TaskID, "switch must follow B's existing task")
			require.NotEqual(t, warm.TurnID, b1.TurnID, "switch must not reuse B's existing turn")
			require.NotEqual(t, a1.TaskID, b1.TaskID)
			require.NotEqual(t, a1.TurnID, b1.TurnID)
			b2 := send(b, 2, false)
			require.Equal(t, b1.TaskID, b2.TaskID)
			require.Equal(t, b1.TurnID, b2.TurnID)
			back := send(a, 2, true)
			require.Equal(t, a1.TaskID, back.TaskID, "return must retain A's unexpired task")
			require.NotEqual(t, a1.TurnID, back.TurnID, "A-B-A must never reuse A's old turn")
			require.NotEqual(t, b1.TurnID, back.TurnID)
			require.EqualValues(t, 2, back.Generation)
			if mode == auth.CodexFingerprintModeTurnRound {
				require.Equal(t, "1", b1.AgentIteration)
				require.Equal(t, "2", b2.AgentIteration)
				require.Equal(t, "1", back.AgentIteration)
			} else {
				require.Equal(t, "2", b1.AgentIteration, "task rounds retain the target account's counter")
				require.Equal(t, "3", b2.AgentIteration)
				require.Equal(t, "3", back.AgentIteration)
			}
			h.continuityRecords = nil // Restore the migration segment from storage.
			retry := send(a, 2, false)
			require.Equal(t, back.TaskID, retry.TaskID)
			require.Equal(t, back.TurnID, retry.TurnID)
			require.Equal(t, back.AgentIteration, retry.AgentIteration)
			require.True(t, retry.ReusedStep)
		})
	}
}

func TestBPSConvergenceSoftAffinityReturnUsesFreshTurn(t *testing.T) {
	for _, mode := range []string{auth.CodexFingerprintModeRound, auth.CodexFingerprintModeTurnRound} {
		t.Run(mode, func(t *testing.T) {
			h, a, b, _ := failoverTestSetup(t, false)
			h.store.SetAffinityMode("strict")
			for _, account := range []*auth.Account{a, b} {
				account.CodexBPS, account.CodexFingerprintMode = true, mode
			}
			body := `{"model":"gpt-5.6-sol","input":"same prompt"}`
			send := func(account *auth.Account) *bpsWordIdentityDiagnostic {
				t.Helper()
				c := inferredSessionFixture(t, body, "user", "device", "client/1", 8)
				beginDispatchSelection(c)
				ctx := WithCodexIdentityStore(c.Request.Context(), h.db)
				selected, _, _ := h.nextAccountForBPSTask(ctx, "", 0, nil, func(candidate *auth.Account) bool { return candidate.ID() == account.ID() }, auth.DispatchPolicyStandard.WithModel("gpt-5.6-sol"))
				require.Same(t, account, selected)
				h.store.Release(selected)
				ctx, err := withBPSFullConvergence(ctx, account, bpsProfile(auth.BPSWord), nil, nil, "cache", "key")
				require.NoError(t, err)
				d, err := resolveBPSWordIdentity(ctx, []byte(body), nil, "cache", "gpt-5.6-sol", false)
				require.NoError(t, err)
				return d
			}
			a1 := send(a)
			aRetry := send(a)
			require.Equal(t, a1.TurnID, aRetry.TurnID)
			require.True(t, aRetry.ReusedStep)
			b1 := send(b)
			require.NotEqual(t, a1.TaskID, b1.TaskID)
			require.NotEqual(t, a1.TurnID, b1.TurnID)
			bRetry := send(b)
			require.Equal(t, b1.TurnID, bRetry.TurnID, "the persisted switch revision must be used immediately")
			require.Equal(t, b1.AgentIteration, bRetry.AgentIteration)
			back := send(a)
			require.Equal(t, a1.TaskID, back.TaskID)
			require.NotEqual(t, a1.TurnID, back.TurnID)
			backRetry := send(a)
			require.Equal(t, back.TurnID, backRetry.TurnID)
			require.Equal(t, back.AgentIteration, backRetry.AgentIteration)
			if mode == auth.CodexFingerprintModeTurnRound {
				require.Equal(t, "1", back.AgentIteration)
			}
		})
	}
}

func TestBPSConvergenceTemporaryFailoverChangesEpoch(t *testing.T) {
	h, a, b, _ := failoverTestSetup(t, false)
	a.CodexBPS, b.CodexBPS = true, true
	a.CodexFingerprintMode, b.CodexFingerprintMode = auth.CodexFingerprintModeRound, auth.CodexFingerprintModeRound
	c := transportTestContext()
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), codexRouteFloorKey{}, "bps"))
	state := &relaxedAccountFallback{key: "temporary-request", accounts: map[int64]bool{}}
	var first string
	for i, account := range []*auth.Account{a, a, b, a} {
		require.Nil(t, h.commitRelaxedAccountFallback(c, account, state))
		epoch := outboundEpochFromContext(c.Request.Context())
		require.True(t, epoch.temporary)
		require.Equal(t, []uint64{1, 1, 2, 3}[i], epoch.record.FailoverCount)
		if i == 0 {
			first = epoch.identityKey()
		}
		if i == 1 {
			require.Equal(t, first, epoch.identityKey())
		}
		if i == 3 {
			require.NotEqual(t, first, epoch.identityKey())
		}
	}
	_, found, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(state.key))
	require.NoError(t, err)
	require.False(t, found, "temporary segments must not claim persistent root ownership")
}
