package proxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
)

func TestCodexDualRouteSelectionAndInheritance(t *testing.T) {
	native := true
	a := &auth.Account{DBID: 11, CodexNative: &native, CodexBPS: true, CodexNativeModels: []string{"gpt-5.6-*"}, CodexBPSModels: []string{"gpt-6-*"}}
	for _, tc := range []struct {
		model, prior, want string
		auxiliary          bool
	}{
		{"gpt-5.6-sol", "", "native", false},
		{"gpt-6-astra", "", "bps", false},
		{"gpt-6-astra", "native", "bps", false},
		{"gpt-5.6-sol", "bps", "", false},
		{"codex-auto-review", "bps", "bps", true},
		{"gpt-6-astra", "native", "native", true},
	} {
		require.Equal(t, tc.want, selectCodexRoute(a, tc.model, tc.prior, tc.auxiliary))
	}
	ctx := context.WithValue(t.Context(), sessionOutboundEpochContextKey{}, &sessionOutboundEpoch{record: database.SessionContinuityRecord{AccountID: a.ID(), UpstreamMode: "bps"}})
	mode, err := codexRequestRouteMode(ctx, a, "gpt-6-astra")
	require.NoError(t, err)
	require.Equal(t, "bps", mode)
	require.Error(t, ValidateCodexNativeRoute(ctx, a, []byte(`{"model":"gpt-6-astra"}`)))
	a.CodexBPS = false
	_, err = codexRequestRouteMode(ctx, a, "gpt-6-astra")
	require.Error(t, err) // Disabling BPS cannot return the pinned root to native.
}

func TestCodexRouteFailoverOriginalBPSAndNoReverse(t *testing.T) {
	for _, scenario := range []string{"original_bps", "other_bps", "entire_account_disabled", "bps_no_native", "bps_other_bps", "failover_disabled", "model_switch"} {
		t.Run(scenario, func(t *testing.T) {
			h, owner, target, key := failoverTestSetup(t, scenario != "failover_disabled")
			on, off := true, false
			owner.CodexNative, owner.CodexBPS = &off, true
			target.CodexNative, target.CodexBPS = &off, true
			want := owner
			model := "gpt-5.6-sol"
			switch scenario {
			case "other_bps":
				owner.CodexBPS = false
				want = target
			case "entire_account_disabled":
				atomic.StoreInt32(&owner.Disabled, 1)
				want = target
			case "bps_no_native", "bps_other_bps":
				owner.CodexNative, owner.CodexBPS = &on, false
				record, _, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
				require.NoError(t, err)
				record.UpstreamMode = "bps"
				key += "-bps"
				h.store.BindSessionAffinity(key, owner, "")
				_, err = h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(key), record)
				require.NoError(t, err)
				if scenario == "bps_no_native" {
					target.CodexNative, target.CodexBPS = &on, false
					want = nil
				} else {
					want = target
				}
			case "failover_disabled":
				want = nil
			case "model_switch":
				owner.CodexNative = &on
				owner.CodexNativeModels, owner.CodexBPSModels = []string{"gpt-5.6-*"}, []string{"gpt-6-*"}
				owner.Models, target.Models = nil, nil
				model = "gpt-6-astra"
			}
			c, body := failoverTestRequest(t, h)
			err := h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, model, model, false, body)
			require.Nil(t, err)
			oldEpoch := c.Request.Context()
			selected, _, handled := h.takeSessionAccountFailover(c.Request.Context(), key, 0, nil, nil, auth.DispatchPolicyStandard)
			require.Same(t, want, selected)
			if selected == nil {
				require.Equal(t, scenario != "failover_disabled", handled)
				_, dispatchErr := codexRequestRouteMode(c.Request.Context(), owner, model)
				require.Error(t, dispatchErr)
				return
			}
			h.store.Release(selected)
			require.True(t, handled)
			record, _, readErr := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
			require.NoError(t, readErr)
			require.Equal(t, "bps", record.UpstreamMode)
			require.Equal(t, uint64(1), record.FailoverCount)
			require.Equal(t, selected.ID(), record.AccountID)
			require.Error(t, validateSessionOutboundEpoch(oldEpoch, owner))
			require.NoError(t, validateSessionOutboundEpoch(c.Request.Context(), selected))
			if scenario == "bps_other_bps" {
				require.Equal(t, bpsCodexCompactionDomain, requestCompactionDomain(oldEpoch, owner))
			} else {
				require.Equal(t, nativeCodexCompactionDomain, requestCompactionDomain(oldEpoch, owner))
			}
		})
	}
}

func TestCodexRouteCompactionCannotReturnToNative(t *testing.T) {
	h := newWindowAuthorizationHandler(t)
	on := true
	a := &auth.Account{DBID: 12, CodexNative: &on, CodexBPS: true}
	ctx := context.WithValue(t.Context(), sessionOutboundEpochContextKey{}, &sessionOutboundEpoch{record: database.SessionContinuityRecord{AccountID: a.ID(), UpstreamMode: "bps"}})
	require.NoError(t, h.recordCompactionProvenance(ctx, a, "bps-cipher"))
	nativeContext := context.WithValue(t.Context(), sessionOutboundEpochContextKey{}, &sessionOutboundEpoch{record: database.SessionContinuityRecord{AccountID: a.ID(), UpstreamMode: "native"}})
	require.NoError(t, h.recordCompactionProvenance(nativeContext, a, "native-cipher"))
	resolution, err := h.resolveCompactionAffinity(t.Context(), []byte(`{"input":[{"type":"compaction","encrypted_content":"native-cipher"},{"type":"compaction","encrypted_content":"bps-cipher"}]}`))
	require.NoError(t, err)
	require.Equal(t, bpsCodexCompactionDomain, resolution.CompatibilityDomain)
	require.False(t, compactionDomainFilter(resolution.CompatibilityDomain, nil)(&auth.Account{}))
	c, _ := continuityTestRequest(0, "turn")
	applyCompactionRouteFloor(c, resolution)
	mode, err := codexRequestRouteMode(c.Request.Context(), a)
	require.NoError(t, err)
	require.Equal(t, "bps", mode)
}

func TestCodexRouteForkInheritsBPSOrigin(t *testing.T) {
	h, owner, _, _ := failoverTestSetup(t, true)
	on := true
	owner.CodexNative, owner.CodexBPS = &on, true
	c, body := failoverTestRequest(t, h)
	sourceKey := sessionAffinityKey("fork-origin", requestAPIKeyID(c))
	_, err := h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(sourceKey), database.SessionContinuityRecord{AccountID: owner.ID(), UpstreamMode: "bps", ThreadID: continuityTestThread})
	require.NoError(t, err)
	require.Nil(t, h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true, forkSourceAffinityID: "fork-origin"}, "fork-target", "gpt-5.6-sol", "gpt-5.6-sol", false, body))
	require.Nil(t, h.commitSessionContinuity(c, owner))
	mode, err := codexRequestRouteMode(c.Request.Context(), owner, "gpt-5.6-sol")
	require.NoError(t, err)
	require.Equal(t, "bps", mode)
	// Auxiliary models inherit the durable path, bypassing main-model routing.
	owner.CodexBPSModels = []string{"gpt-5.6-sol"}
	ctx := context.WithValue(c.Request.Context(), codexRouteRequestKey{}, codexRouteRequest{Model: "codex-auto-review", Auxiliary: true})
	mode, err = codexRequestRouteMode(ctx, owner)
	require.NoError(t, err)
	require.Equal(t, "bps", mode)
}

func TestCodexDualRouteActualHTTPDispatch(t *testing.T) {
	t.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
	for _, tc := range []struct{ model, url string }{
		{"gpt-5.6-sol", "https://chatgpt.com/backend-api/codex/responses"},
		{"gpt-6-astra", CodexBPSBaseURL + "/responses"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			h := newWindowAuthorizationHandler(t)
			on := true
			a := &auth.Account{DBID: 11, AccessToken: "test-token", AccountID: accountIdentitySampleAccount, CodexNative: &on, CodexBPS: true, CodexNativeModels: []string{"gpt-5.6-*"}, CodexBPSModels: []string{"gpt-6-*"}}
			calls := 0
			installClaudeBoundaryTransport(t, a, func(r *http.Request) (*http.Response, error) {
				calls++
				require.Equal(t, tc.url, r.URL.String())
				body, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				require.NotContains(t, string(body), "upstream_route")
				require.NotContains(t, string(body), "codex_native_models")
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"resp_test","output":[]}`)), Request: r}, nil
			})
			headers, body := accountIdentityFixture(t, false, true)
			body, err := sjson.SetBytes(body, "model", tc.model)
			require.NoError(t, err)
			resp, err := ExecuteRequest(WithCodexIdentityStore(t.Context(), h.db), a, body, "test-session", "", "test-key", nil, headers, false)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, 1, calls)
		})
	}
}
