package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy/degradejudge"
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
		body := readUpstreamRequestBody(r)
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

// pelicanSSE streams html as output_text deltas, reporting model.
func pelicanSSE(model, html string) string {
	var b strings.Builder
	b.WriteString("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_p\",\"model\":\"" + model + "\"}}\n\n")
	for len(html) > 0 {
		chunk := html[:min(len(html), 64)]
		html = html[len(chunk):]
		delta, _ := json.Marshal(map[string]string{"type": "response.output_text.delta", "delta": chunk})
		b.WriteString("data: " + string(delta) + "\n\n")
	}
	b.WriteString("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_p\",\"model\":\"" + model + "\",\"output\":[]}}\n\n")
	return b.String()
}

// pelicanHTML scores exactly score (one circle per point).
func pelicanHTML(score int) string {
	return "<!DOCTYPE html><html><body><svg>" + strings.Repeat("<circle/>", score) + "</svg></body></html>"
}

// pelicanUpstream answers pelican probes with *reply and anything else with
// a plain native stream.
func pelicanUpstream(reply *atomic.Value) func([]byte) (int, string) {
	return func(body []byte) (int, string) {
		var decoded any
		_ = json.Unmarshal(body, &decoded)
		if strings.Contains(fmt.Sprint(decoded), "鹈鹕") {
			return 200, reply.Load().(string)
		}
		return 200, nativeSSE(gjson.GetBytes(body, "model").String())
	}
}

func TestModelMismatchOnlySchedulesAConfirmProbe(t *testing.T) {
	f := dualRouteFixture(t, func([]byte) (int, string) { return 200, "" }, func([]byte) (int, string) { return 200, "" })
	store := f.handler.bpsCache()
	row := nativeBreakerMismatchRow(f.account.ID(), "gpt-5.5", "gpt-5.4-mini")
	f.handler.observeRouteHealth(row)
	f.handler.observeRouteHealth(row)
	rt := f.handler.degradeRuntime()
	require.Len(t, rt.signals, 2, "the client path only enqueues")
	for range 2 {
		f.handler.handleRouteSignal(context.Background(), <-rt.signals)
	}
	require.False(t, nativeRouteOpen(context.Background(), store, f.account), "a mismatch never breaks the route by itself")
	require.Equal(t, "queued", f.handler.DegradeProbePending(f.account.ID(), RouteNative), "it queues one confirm probe")
	require.Len(t, rt.queue, 1, "deduplicated per account and route")
	require.Equal(t, DegradeTriggerMismatch, rt.queue[0].trigger)

	// Unknown and variant-only replies are no signal at all.
	f.handler.observeRouteHealth(&database.UsageLogInput{AccountID: f.account.ID(), StatusCode: 200, Model: "gpt-5.5", EffectiveModel: "gpt-5.5"})
	f.handler.observeRouteHealth(nativeBreakerMismatchRow(f.account.ID(), "gpt-5.5", "gpt-5.5-2026-05-01"))
	require.Empty(t, rt.signals)
}

func TestNative403BreaksTheNativeRouteAtOnce(t *testing.T) {
	f := dualRouteFixture(t, func([]byte) (int, string) { return 200, "" }, func([]byte) (int, string) { return 200, "" })
	f.handler.observeRouteHealth(&database.UsageLogInput{AccountID: f.account.ID(), StatusCode: http.StatusForbidden, Model: "gpt-5.5", EffectiveModel: "gpt-5.5"})
	f.handler.handleRouteSignal(context.Background(), <-f.handler.degradeRuntime().signals)
	require.True(t, nativeRouteOpen(context.Background(), f.handler.bpsCache(), f.account))
	status := BPSAccountStatusesWith(context.Background(), f.handler.bpsCache(), []int64{f.account.ID()}, f.store.FindByID)[0]
	require.Equal(t, "open", status.NativeRoute)
	require.Equal(t, degradeBreakTrigger403, status.NativeTrigger)
	active, _, err := f.db.ListBPSPolicyBlocks(context.Background(), 10)
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, database.BPSRouteNative, active[0].Route)
	require.Equal(t, database.RouteBlockDegrade, active[0].Kind)
}

func TestPelicanVerdictBreaksAndRecoversTheNativeRoute(t *testing.T) {
	var reply atomic.Value
	reply.Store(pelicanSSE("gpt-6-astra", pelicanHTML(186)))
	f := dualRouteFixture(t, func([]byte) (int, string) { return 200, "" }, pelicanUpstream(&reply))
	store := f.handler.bpsCache()

	result := f.handler.RunDegradeProbe(context.Background(), f.account, RouteNative, DegradeTriggerMismatch)
	require.Equal(t, degradejudge.VerdictDegraded, result.Verdict, "186 is degraded: %+v", result)
	require.Equal(t, 186, result.Score)
	require.Equal(t, "gpt-6-astra", result.UpstreamModel)
	require.True(t, nativeRouteOpen(context.Background(), store, f.account), "a degraded verdict breaks the native route")
	req, _, ctx := bpsRoutingRequest(f.handler, `{"model":"gpt-6-sol","input":"hi"}`)
	require.True(t, bpsPlugin{}.Select(ctx, plugins.Attempt{Request: req, Account: f.account, Model: "gpt-6-sol"}), "BPS serves the account")
	require.True(t, BPSOwnsAccount(f.account), "background jobs keep off native")

	probes, err := f.db.ListDegradeProbes(context.Background(), database.DegradeProbeFilter{AccountID: f.account.ID()})
	require.NoError(t, err)
	require.Len(t, probes, 1)
	stored, err := f.db.GetDegradeProbe(context.Background(), probes[0].ID)
	require.NoError(t, err)
	require.Equal(t, pelicanHTML(186), stored.HTML, "the sample is kept for review")

	// A recovery probe that scores 187 and reports the requested model closes it.
	reply.Store(pelicanSSE("gpt-6-astra", pelicanHTML(187)))
	result = f.handler.RunDegradeProbe(context.Background(), f.account, RouteNative, DegradeTriggerRecovery)
	require.Equal(t, degradejudge.VerdictOK, result.Verdict, "187 is not degraded")
	require.False(t, nativeRouteOpen(context.Background(), store, f.account))
	_, history, err := f.db.ListBPSPolicyBlocks(context.Background(), 10)
	require.NoError(t, err)
	require.Len(t, history, 1)
	require.Equal(t, database.BPSRouteNative, history[0].Route)

	// A high score from another model is still degraded.
	reply.Store(pelicanSSE("gpt-5.4-mini", pelicanHTML(240)))
	result = f.handler.RunDegradeProbe(context.Background(), f.account, RouteNative, DegradeTriggerScheduled)
	require.Equal(t, degradejudge.VerdictDegraded, result.Verdict)
	require.True(t, nativeRouteOpen(context.Background(), store, f.account))
}

func TestInvalidPelicanSampleRetriesOnceAndNeverCounts(t *testing.T) {
	var reply atomic.Value
	reply.Store(pelicanSSE("gpt-6-astra", "I cannot draw that."))
	f := dualRouteFixture(t, func([]byte) (int, string) { return 200, "" }, pelicanUpstream(&reply))
	result := f.handler.RunDegradeProbe(context.Background(), f.account, RouteNative, DegradeTriggerManual)
	require.Equal(t, degradejudge.VerdictInvalid, result.Verdict)
	require.EqualValues(t, 2, f.native.Load(), "one retry")
	require.False(t, nativeRouteOpen(context.Background(), f.handler.bpsCache(), f.account), "an invalid sample never counts")

	// During recovery an invalid sample retries a minute later.
	f.handler.breakRoute(context.Background(), f.account, RouteNative, degradeBreakTrigger403, "403 · gpt-5.5", "gpt-6-astra")
	f.handler.RunDegradeProbe(context.Background(), f.account, RouteNative, DegradeTriggerRecovery)
	record := bpsAccountStateStore.load(context.Background(), f.handler.bpsCache(), routeBreakerKey(RouteNative, f.account.ID()), time.Now())
	require.True(t, record.NeedsProbe)
	require.WithinDuration(t, time.Now().Add(degradeInvalidRetry), record.NextProbe, 5*time.Second)
}

func TestNativeProbeRefusedWithoutAnExplicitNativeRoute(t *testing.T) {
	freshBPSAccountStates(t)
	f := newBPSHandlerFixture(t, map[string]any{auth.CodexBPSEnabledCredentialKey: true})
	on := true
	f.store.ApplyAccountTransportPluginOverride(f.account.ID(), BPSPluginID, &on)
	require.ErrorIs(t, checkDegradeRoute(f.account, RouteNative), errDegradeNativeForbidden)
	result := f.handler.RunDegradeProbe(context.Background(), f.account, RouteNative, DegradeTriggerManual)
	require.Equal(t, degradejudge.VerdictInvalid, result.Verdict)
	require.Contains(t, result.Error, "未显式开启原生路由")
	require.Zero(t, f.native.Load(), "no native request was sent")
	require.NoError(t, checkDegradeRoute(f.account, RouteBPS), "its BPS route can be probed")
}

func TestDegradedBPSRouteIsVetoedAndNativeServes(t *testing.T) {
	var reply atomic.Value
	reply.Store(pelicanSSE("gpt-6-astra", pelicanHTML(120)))
	f := dualRouteFixture(t, func(body []byte) (int, string) { return 200, reply.Load().(string) }, func([]byte) (int, string) { return 200, "" })
	result := f.handler.RunDegradeProbe(context.Background(), f.account, RouteBPS, DegradeTriggerScheduled)
	require.Equal(t, degradejudge.VerdictDegraded, result.Verdict)
	require.EqualValues(t, 1, f.bps.Load())
	require.True(t, routeBreakerOpen(context.Background(), f.handler.bpsCache(), f.account, RouteBPS))
	req, state, ctx := bpsRoutingRequest(f.handler, `{"model":"gpt-6-sol","input":"hi"}`)
	require.Equal(t, BPSDegradedReason, state.blockReason(ctx, f.account, "gpt-6-sol"))
	require.Nil(t, plugins.Default().Resolve(ctx, req, f.account, "gpt-6-sol", plugins.KindResponses), "the native route serves")
	status := BPSAccountStatusesWith(context.Background(), f.handler.bpsCache(), []int64{f.account.ID()}, f.store.FindByID)[0]
	require.True(t, status.BPSDegraded)
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
	require.Nil(t, plugins.Default().Resolve(ctx, req, f.account, "gpt-6-sol", plugins.KindResponses))
	transport, meta := req.Transport()
	require.Equal(t, database.TransportNative, transport)
	require.Contains(t, meta, BPSPolicyBlockedKind, "the native attempt records why it was rerouted")

	f.handler.breakRoute(context.Background(), f.account, RouteNative, degradeBreakTrigger403, "403 · gpt-5.5", "")
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

// The same-account native retry never targets an account whose native route
// is broken.
func TestNoSameAccountRetryWhenTheNativeRouteIsBroken(t *testing.T) {
	f := dualRouteFixture(t, func([]byte) (int, string) { return 200, "" }, func([]byte) (int, string) { return 200, "" })
	f.handler.breakRoute(context.Background(), f.account, RouteNative, degradeBreakTrigger403, "403 · gpt-5.5", "")
	req, _, ctx := bpsRoutingRequest(f.handler, `{"model":"gpt-6-sol","input":"hi"}`)
	env := &plugins.ReqEnv{Request: req, Account: f.account, Model: "gpt-6-sol"}
	bpsPlugin{}.FilterHeaders(env, http.Header{})
	_, err := bpsPlugin{}.TransformJSON(env, http.StatusForbidden, []byte(bpsPolicyBlockBody))
	require.NoError(t, err)
	require.Zero(t, bpsPlugin{}.RetryAccount(ctx, req))
}

// Hook no-op guarantees: account selection keeps its exclusions for
// native-only and BPS-only accounts, and the usage-row detector never touches
// the row nor enqueues for accounts it does not apply to.
func TestBreakerHooksAreNoOpsOutsideTheirCondition(t *testing.T) {
	freshBPSAccountStates(t)
	nativeOnly := &auth.Account{DBID: 9801, AccountID: "native-only", AccessToken: "at", Status: auth.StatusReady}
	bpsOnly := withBPSOverride(&auth.Account{DBID: 9802, AccountID: "bps-only", AccessToken: "at", Status: auth.StatusReady}, true)
	spare := &auth.Account{DBID: 9803, AccountID: "spare", AccessToken: "at", Status: auth.StatusReady}
	handler := bpsRoutingHandler(t, nativeOnly, bpsOnly, spare)
	req, _, ctx := bpsRoutingRequest(handler, `{"model":"gpt-6-sol","input":"hi"}`)
	env := &plugins.ReqEnv{Request: req, Account: bpsOnly, Model: "gpt-6-sol"}
	bpsPlugin{}.FilterHeaders(env, http.Header{})
	_, err := bpsPlugin{}.TransformJSON(env, http.StatusForbidden, []byte(bpsPolicyBlockBody))
	require.NoError(t, err)
	require.Zero(t, plugins.Default().RetryAccount(ctx, req), "a BPS-only account has no native route to retry on")

	exclusions := newRetryAccountExclusions()
	exclusions.MarkHard(nativeOnly.ID())
	exclusions.MarkHard(bpsOnly.ID())
	before := exclusions.ForSelection()
	account, _, _, err := handler.nextRetryAccountWithGuard(ctx, "", 0, exclusions, nil, false, auth.DispatchPolicyStandard)
	require.NoError(t, err)
	require.NotNil(t, account)
	require.Equal(t, spare.ID(), account.ID())
	handler.store.Release(account)
	require.Equal(t, before, exclusions.ForSelection(), "exclusions are unchanged")

	for _, row := range []*database.UsageLogInput{
		nativeBreakerMismatchRow(nativeOnly.ID(), "gpt-5.5", "gpt-5.4-mini"),
		{AccountID: nativeOnly.ID(), StatusCode: http.StatusForbidden, Model: "gpt-5.5"},
		nativeBreakerMismatchRow(bpsOnly.ID(), "gpt-5.5", "gpt-5.4-mini"), // native row of a BPS-only account
	} {
		raw, _ := json.Marshal(row)
		handler.observeRouteHealth(row)
		after, _ := json.Marshal(row)
		require.Equal(t, string(raw), string(after), "the row is untouched")
	}
	require.Empty(t, handler.degradeRuntime().signals, "nothing is enqueued for accounts the breaker does not apply to")

	off := false
	updateBPSConfig(t, func(c BPSConfig) BPSConfig { c.DegradeBreakerEnabled = &off; return c })
	handler.observeRouteHealth(nativeBreakerMismatchRow(bpsOnly.ID(), "gpt-6-sol", "gpt-5.4-mini"))
	require.Empty(t, handler.degradeRuntime().signals, "nothing when the breaker is off")
}

func TestDegradeConfig(t *testing.T) {
	cfg, err := parseBPSConfig(nil)
	require.NoError(t, err)
	require.True(t, cfg.DegradeEnabled())
	require.Equal(t, 187, cfg.DegradeThreshold())
	require.Equal(t, "gpt-6-astra", cfg.DegradeModel())
	require.Zero(t, cfg.DegradeInterval(), "sampling is off by default")
	require.Equal(t, 2, cfg.DegradeMaxConcurrent())
	require.Equal(t, cfg.PolicyLadder(), cfg.DegradeLadder())
	cfg, err = parseBPSConfig([]byte(`{"degrade_breaker_enabled":false,"degrade_score_threshold":200,"degrade_probe_model":"gpt-6-sol","degrade_probe_interval":"6h","degrade_probe_max_concurrent":4,"degrade_cooldown_ladder":["5m","1h"]}`))
	require.NoError(t, err)
	require.False(t, cfg.DegradeEnabled())
	require.Equal(t, 200, cfg.DegradeThreshold())
	require.Equal(t, 6*time.Hour, cfg.DegradeInterval())
	require.Equal(t, 4, cfg.DegradeMaxConcurrent())
	require.Equal(t, []time.Duration{5 * time.Minute, time.Hour}, cfg.DegradeLadder())
	for _, bad := range []string{`{"degrade_probe_interval":"1m"}`, `{"degrade_probe_max_concurrent":17}`, `{"degrade_cooldown_ladder":["soon"]}`, `{"degrade_score_threshold":-1}`} {
		_, err := parseBPSConfig([]byte(bad))
		require.Error(t, err, bad)
	}
}

func TestDegradeQueueRespectsMaxConcurrency(t *testing.T) {
	handler := bpsRoutingHandler(t)
	for id := int64(1); id <= 3; id++ {
		require.True(t, handler.EnqueueDegradeProbe(id, RouteBPS, DegradeTriggerManual))
	}
	require.False(t, handler.EnqueueDegradeProbe(1, RouteBPS, DegradeTriggerManual), "deduplicated")
	rt := handler.degradeRuntime()
	first, ok := rt.takeDegradeJob(2)
	require.True(t, ok)
	_, ok = rt.takeDegradeJob(2)
	require.True(t, ok)
	_, ok = rt.takeDegradeJob(2)
	require.False(t, ok, "two in flight at most")
	require.Equal(t, "running", handler.DegradeProbePending(first.accountID, RouteBPS))
	rt.finishDegradeJob(first)
	_, ok = rt.takeDegradeJob(2)
	require.True(t, ok, "a finished probe frees a slot")
}
