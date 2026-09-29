package proxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy/plugins"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func nativeBreakerMismatchRow(accountID int64, requested, reported string) *database.UsageLogInput {
	mismatch := true
	return &database.UsageLogInput{AccountID: accountID, StatusCode: 200, Model: requested, EffectiveModel: requested, UpstreamResponseModel: reported, UpstreamModelMismatch: &mismatch}
}

// dualRouteFixture is a gateway whose account has BPS and an explicit native
// route; bps / native script the two upstreams.
func dualRouteFixture(t *testing.T, bps, native func(body []byte) (int, string)) *bpsHandlerFixture {
	t.Helper()
	freshBPSAccountStates(t)
	f := newBPSHandlerFixture(t, map[string]any{auth.CodexBPSEnabledCredentialKey: true, auth.CodexNativeEnabledCredentialKey: true})
	on := true
	f.store.ApplyAccountTransportPluginOverride(f.account.ID(), BPSPluginID, &on)
	f.account.SetCodexBPSOptions(auth.CodexBPSAccountOptions{Native: &on, ImageTrim: true})
	require.True(t, f.account.CodexNativeRouteExplicit())
	installClaudeBoundaryTransport(t, f.account, func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		reply := native
		if strings.HasPrefix(r.URL.String(), CodexBPSBaseURL) {
			f.bps.Add(1)
			reply = bps
		} else {
			f.native.Add(1)
		}
		status, payload := reply(body)
		contentType := "text/event-stream"
		if status >= 300 {
			contentType = "application/json"
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(payload)), Request: r}, nil
	})
	return f
}

func nativeSSE(model string) string {
	return "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_n\",\"model\":\"" + model + "\"}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"m\",\"delta\":\"OK\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_n\",\"model\":\"" + model + "\",\"output\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":1,\"total_tokens\":4}}}\n\n"
}

func TestNativeModelMismatchIgnoresGatewayAliases(t *testing.T) {
	for _, tc := range []struct {
		requested, effective, reported string
		want                           bool
	}{
		{"gpt-5.5", "gpt-5.5", "gpt-5.4-mini", true},
		{"gpt-5.5", "gpt-5.5", "GPT-5.5", false},
		{"gpt-5.5", "gpt-5.5", "gpt-5.5-2026-05-01", false},
		{"gpt-5.5", "gpt-5.5", "gpt-5.5-latest", false},
		{"codex-auto-review", "codex-auto-review", "gpt-5.6-luna", false},
		{"my-alias", "gpt-5.5", "gpt-5.5", false},                   // account model mapping
		{"claude-haiku-4-5", "gpt-5.6-luna", "gpt-5.6-luna", false}, // gateway model mapping
		{"gpt-5.5", "gpt-5.5", "", false},                           // not reported: unknown
	} {
		require.Equal(t, tc.want, nativeModelMismatch(tc.requested, tc.effective, tc.reported), "%+v", tc)
	}
}

func TestNativeBreakerOpensOnModelMismatchesAndBPSServes(t *testing.T) {
	f := dualRouteFixture(t, func([]byte) (int, string) { return 200, "" }, func([]byte) (int, string) { return 200, "" })
	store := f.handler.bpsCache()
	unknown := &database.UsageLogInput{AccountID: f.account.ID(), StatusCode: 200, Model: "gpt-5.5", EffectiveModel: "gpt-5.5"}
	f.handler.observeNativeRouteHealth(nil, unknown)
	f.handler.observeNativeRouteHealth(nil, nativeBreakerMismatchRow(f.account.ID(), "gpt-5.5", "gpt-5.5-2026-05-01"))
	require.False(t, nativeRouteOpen(context.Background(), store, f.account), "unknown and variant-only replies never count")

	f.handler.observeNativeRouteHealth(nil, nativeBreakerMismatchRow(f.account.ID(), "gpt-5.5", "gpt-5.4-mini"))
	require.False(t, nativeRouteOpen(context.Background(), store, f.account), "1 of 2")
	f.handler.observeNativeRouteHealth(nil, nativeBreakerMismatchRow(f.account.ID(), "gpt-5.5", "gpt-5.4-mini"))
	require.True(t, nativeRouteOpen(context.Background(), store, f.account), "2 mismatches in 10 minutes open the native route")

	status := BPSAccountStatusesWith(context.Background(), store, []int64{f.account.ID()}, f.store.FindByID)[0]
	require.Equal(t, "open", status.NativeRoute)
	require.Equal(t, NativeTriggerModel, status.NativeTrigger)
	require.Equal(t, "gpt-5.5 → gpt-5.4-mini", status.NativeDetail)
	active, _, err := f.db.ListBPSPolicyBlocks(context.Background(), 10)
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, database.BPSRouteNative, active[0].Route)
	require.Contains(t, active[0].Detail, "gpt-5.5 → gpt-5.4-mini")

	// With the native route open, BPS serves the account (it would have
	// preferred native), and background jobs treat it as BPS-owned.
	req, _, ctx := bpsRoutingRequest(f.handler, `{"model":"gpt-6-sol","input":"hi"}`)
	ok, _ := bpsPlugin{}.Admissible(ctx, f.account, "gpt-6-sol")
	require.True(t, ok)
	require.True(t, bpsPlugin{}.Select(ctx, plugins.Attempt{Request: req, Account: f.account, Model: "gpt-6-sol"}))
	require.True(t, BPSOwnsAccount(f.account))

	// A native-only account (BPS off) only records the signal.
	nativeOnly := (&auth.Account{DBID: 9701, AccountID: "native-only", AccessToken: "at"}).SetCodexBPSOptions(auth.CodexBPSAccountOptions{Native: boolPtrForUpstreamModelTest(true)})
	f.store.AddAccount(nativeOnly)
	for range 3 {
		f.handler.observeNativeRouteHealth(nil, nativeBreakerMismatchRow(nativeOnly.ID(), "gpt-5.5", "gpt-5.4-mini"))
	}
	require.False(t, nativeRouteOpen(context.Background(), store, nativeOnly), "no BPS route to fall back on: the breaker never opens")
}

func TestNativeBreakerOpensOnOneNative403AndTheProbeClosesIt(t *testing.T) {
	var reply atomic.Value
	reply.Store(nativeSSE("gpt-5.4-mini"))
	f := dualRouteFixture(t, func([]byte) (int, string) { return 200, "" }, func(body []byte) (int, string) {
		return 200, reply.Load().(string)
	})
	store := f.handler.bpsCache()
	f.handler.observeNativeRouteHealth(nil, &database.UsageLogInput{AccountID: f.account.ID(), StatusCode: http.StatusForbidden, Model: "gpt-5.5", EffectiveModel: "gpt-5.5"})
	require.True(t, nativeRouteOpen(context.Background(), store, f.account), "one native 403 opens the route")

	record := bpsAccountStateStore.load(context.Background(), store, nativeRouteStateKey(f.account.ID()), time.Now())
	require.Equal(t, NativeTrigger403, record.Trigger)
	require.Equal(t, 1, record.PolicyTier)
	// The probe is due when the tier ends; a still-degraded reply climbs the ladder.
	require.Equal(t, nativeProbeDegraded, f.handler.probeNativeRoute(context.Background(), f.account, record))
	record = bpsAccountStateStore.load(context.Background(), store, nativeRouteStateKey(f.account.ID()), time.Now())
	require.Equal(t, 2, record.PolicyTier)
	require.Equal(t, "gpt-5.5 → gpt-5.4-mini", record.Detail)
	require.True(t, nativeRouteOpen(context.Background(), store, f.account))

	reply.Store(nativeSSE("gpt-5.5"))
	require.Equal(t, bpsProbeOK, f.handler.probeNativeRoute(context.Background(), f.account, record))
	require.False(t, nativeRouteOpen(context.Background(), store, f.account), "the requested model came back: the breaker closes")
	_, history, err := f.db.ListBPSPolicyBlocks(context.Background(), 10)
	require.NoError(t, err)
	require.Len(t, history, 1)
	require.Equal(t, database.BPSRouteNative, history[0].Route)
	require.Equal(t, 2, history[0].ProbeCount)
}

func TestBothRoutesBrokenVetoesTheAccount(t *testing.T) {
	f := dualRouteFixture(t, func([]byte) (int, string) { return 200, "" }, func([]byte) (int, string) { return 200, "" })
	other := withBPSOverride(&auth.Account{DBID: 9711, AccountID: "other", AccessToken: "at", Status: auth.StatusReady}, true)
	f.store.AddAccount(other)
	store := f.handler.bpsCache()
	for range currentBPSConfig().PolicyBlockThreshold {
		recordBPSFailure(context.Background(), store, f.account.ID(), "gpt-6-sol", http.StatusForbidden, nil, []byte(bpsPolicyBlockBody))
	}
	req, _, ctx := bpsRoutingRequest(f.handler, `{"model":"gpt-6-sol","input":"hi"}`)
	ok, _ := bpsPlugin{}.Admissible(ctx, f.account, "gpt-6-sol")
	require.True(t, ok, "BPS broken, native healthy: the native route serves")
	require.Nil(t, plugins.Default().Resolve(ctx, req, f.account, "gpt-6-sol", plugins.KindResponses), "BPS broken: native serves")
	transport, meta := req.Transport()
	require.Equal(t, database.TransportNative, transport)
	require.Contains(t, meta, BPSPolicyBlockedKind, "the native attempt records why it was rerouted")

	f.handler.observeNativeRouteHealth(nil, &database.UsageLogInput{AccountID: f.account.ID(), StatusCode: http.StatusForbidden, Model: "gpt-5.5", EffectiveModel: "gpt-5.5"})
	_, _, ctx = bpsRoutingRequest(f.handler, `{"model":"gpt-6-sol","input":"hi"}`)
	ok, reason := bpsPlugin{}.Admissible(ctx, f.account, "gpt-6-sol")
	require.False(t, ok, "both routes broken: the account is vetoed")
	require.Equal(t, BPSPolicyBlockedKind, reason)
	ok, _ = bpsPlugin{}.Admissible(ctx, other, "gpt-6-sol")
	require.True(t, ok, "another account takes the request")
}

// A usage-policy 403 on BPS hands the attempt to the same account's native
// route before any other account (once), unless the conversation is pinned.
func TestBPSPolicyBlockRetriesTheSameAccountNatively(t *testing.T) {
	f := dualRouteFixture(t, func([]byte) (int, string) { return 200, "" }, func([]byte) (int, string) { return 200, "" })
	req, state, ctx := bpsRoutingRequest(f.handler, `{"model":"gpt-6-sol","input":"hi"}`)
	env := &plugins.ReqEnv{Request: req, Account: f.account, Model: "gpt-6-sol"}
	bpsPlugin{}.FilterHeaders(env, http.Header{})
	_, err := bpsPlugin{}.TransformJSON(env, http.StatusForbidden, []byte(bpsPolicyBlockBody))
	require.NoError(t, err)
	require.Equal(t, f.account.ID(), bpsPlugin{}.RetryAccount(ctx, req), "the refused account retries natively")
	require.Equal(t, f.account.ID(), bpsPlugin{}.PreferredAccount(ctx, req, "gpt-6-sol"))
	require.Equal(t, f.account.ID(), plugins.Default().RetryAccount(ctx, req))

	exclusions := newRetryAccountExclusions()
	exclusions.MarkHard(f.account.ID())
	exclusions.Reprieve(plugins.Default().RetryAccount(ctx, req))
	require.False(t, exclusions.ForSelection()[f.account.ID()], "its retry exclusion is lifted")
	ok, _ := bpsPlugin{}.Admissible(ctx, f.account, "gpt-6-sol")
	require.True(t, ok)
	require.Nil(t, plugins.Default().Resolve(ctx, req, f.account, "gpt-6-sol", plugins.KindResponses), "the retry goes native")
	_, meta := req.Transport()
	require.Equal(t, "bps_account_refused", gjson.Get(meta, "route_reason").String())
	bpsPlugin{}.AccountSelected(ctx, req, f.account, "gpt-6-sol")
	require.Zero(t, bpsPlugin{}.RetryAccount(ctx, req), "only once")
	require.Len(t, state.nativeRetry, 1)

	// A pinned conversation (bound to BPS output) never moves to native.
	pinnedReq, pinned, pinnedCtx := bpsRoutingRequest(f.handler, `{"model":"gpt-6-sol","input":"hi"}`)
	pinned.pinned = true
	pinnedEnv := &plugins.ReqEnv{Request: pinnedReq, Account: f.account, Model: "gpt-6-sol"}
	bpsPlugin{}.FilterHeaders(pinnedEnv, http.Header{})
	_, err = bpsPlugin{}.TransformJSON(pinnedEnv, http.StatusForbidden, []byte(bpsPolicyBlockBody))
	require.NoError(t, err)
	require.Zero(t, bpsPlugin{}.RetryAccount(pinnedCtx, pinnedReq))
}

// End to end through /v1/responses: two native replies with another model
// open the native route, and the next request is served by BPS on the same
// account.
func TestNativeBreakerEndToEnd(t *testing.T) {
	bpsSSE := "data: {\"type\":\"response.output_text.delta\",\"item_id\":\"m\",\"delta\":\"OK\"}\n\n" + "data: " + bpsFixtureCompleted + "\n\n"
	f := dualRouteFixture(t, func([]byte) (int, string) { return 200, bpsSSE }, func([]byte) (int, string) { return 200, nativeSSE("gpt-5.4-mini") })
	for i := range 2 {
		recorder := f.serve(t, "/v1/responses", `{"model":"gpt-6-sol","stream":true,"input":"hi"}`)
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		require.EqualValues(t, i+1, f.native.Load(), "native keeps precedence while it is healthy")
	}
	f.db.FlushUsageLogs()
	require.True(t, nativeRouteOpen(context.Background(), f.handler.bpsCache(), f.account))
	recorder := f.serve(t, "/v1/responses", `{"model":"gpt-6-sol","stream":true,"input":"hi"}`)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.EqualValues(t, 2, f.native.Load())
	require.EqualValues(t, 1, f.bps.Load(), "the open native route sends the account's traffic to BPS")
	rows := f.usageRows(t)
	require.Equal(t, BPSPluginID, rows[0].Transport)
	require.Equal(t, "native_breaker_open", gjson.Get(rows[0].PluginMeta, "route_reason").String())
}

func TestNativeBreakerConfig(t *testing.T) {
	cfg, err := parseBPSConfig(nil)
	require.NoError(t, err)
	require.True(t, cfg.NativeBreakerEnabled())
	require.Equal(t, 2, cfg.NativeThreshold())
	require.Equal(t, 10*time.Minute, cfg.NativeWindow())
	require.Equal(t, cfg.PolicyLadder(), cfg.NativeLadder(), "the native ladder defaults to the policy ladder")
	cfg, err = parseBPSConfig([]byte(`{"native_degrade_breaker_enabled":false,"native_degrade_threshold":3,"native_degrade_window":"30m","native_cooldown_ladder":["5m","1h"]}`))
	require.NoError(t, err)
	require.False(t, cfg.NativeBreakerEnabled())
	require.Equal(t, 3, cfg.NativeThreshold())
	require.Equal(t, 30*time.Minute, cfg.NativeWindow())
	require.Equal(t, []time.Duration{5 * time.Minute, time.Hour}, cfg.NativeLadder())
	for _, bad := range []string{`{"native_degrade_threshold":21}`, `{"native_degrade_window":"10s"}`, `{"native_cooldown_ladder":["soon"]}`} {
		_, err := parseBPSConfig([]byte(bad))
		require.Error(t, err, bad)
	}
}
