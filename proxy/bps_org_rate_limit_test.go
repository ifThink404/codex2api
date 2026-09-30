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
	"github.com/codex2api/proxy/plugins"
	"github.com/stretchr/testify/require"
)

// The exact texts observed on wlj-01.
const (
	bpsOrgTPMMessage     = "Rate limit reached for gpt-6-sol in organization org-AbCdEf123 on tokens per min (TPM): Limit 40000000, Used 40000000, Requested 19479. Please try again in 29ms."
	bpsAccountRateLimit  = "429: Rate limit exceeded"
	bpsOrgTPMBody        = `{"error":{"message":"` + bpsOrgTPMMessage + `","type":"tokens","param":null,"code":"rate_limit_exceeded"}}`
	bpsAccountRateBody   = `{"error":{"message":"` + bpsAccountRateLimit + `","type":"server_error","param":null,"code":null}}`
	bpsOrgStreamFailure  = `{"type":"response.failed","response":{"id":"resp_org","status":"failed","error":{"code":"rate_limit_exceeded","message":"` + bpsOrgTPMMessage + `"}}}`
	bpsOrgStreamCreated  = `{"type":"response.created","response":{"id":"resp_org","status":"in_progress","output":[]}}`
	bpsOrgStreamComplete = `{"type":"response.completed","response":{"id":"resp_ok","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":10,"output_tokens":1,"total_tokens":11}}}`
)

func TestBPSRateLimitScopeClassification(t *testing.T) {
	for _, tc := range []struct {
		name      string
		message   string
		org       bool
		hint      time.Duration
		retryable bool
	}{
		{name: "org TPM (wlj-01)", message: bpsOrgTPMMessage, org: true, hint: 29 * time.Millisecond, retryable: true},
		{name: "per-account (wlj-01)", message: bpsAccountRateLimit},
		{name: "org in seconds", message: "Rate limit reached for gpt-6-sol in organization org-x on requests per min (RPM): Limit 10, Used 10. Please try again in 1.5s.", org: true, hint: 1500 * time.Millisecond, retryable: true},
		{name: "org without a hint", message: "Rate limit reached for gpt-6-sol in organization org-x on tokens per min (TPM).", org: true},
		{name: "org hint above the cap", message: "Rate limit reached for gpt-6-sol in organization org-x on tokens per min (TPM). Please try again in 12s.", org: true, hint: 12 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hint, org := bpsOrgRateLimitHint(tc.message)
			require.Equal(t, tc.org, org)
			require.Equal(t, tc.hint, hint)
			_, retryable := bpsOrgRetryable(tc.message)
			require.Equal(t, tc.retryable, retryable)
		})
	}
}

type bpsOrgUpstream struct {
	calls   atomic.Int32
	respond func(n int32) (int, string, string)
}

func installBPSOrgUpstream(t *testing.T, account *auth.Account, u *bpsOrgUpstream) {
	installClaudeBoundaryTransport(t, account, func(r *http.Request) (*http.Response, error) {
		_, _ = io.ReadAll(r.Body)
		if !strings.HasPrefix(r.URL.String(), CodexBPSBaseURL) {
			return &http.Response{StatusCode: 500, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("native")), Request: r}, nil
		}
		status, ctype, body := u.respond(u.calls.Add(1))
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {ctype}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
}

func bpsOrgSSE(events ...string) string {
	var b strings.Builder
	for _, event := range events {
		b.WriteString("data: " + event + "\n\n")
	}
	return b.String()
}

func runBPSOrgExecute(t *testing.T, account *auth.Account) (*http.Response, *plugins.ReqEnv, error) {
	t.Helper()
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	req := plugins.NewRequest("req-org", plugins.KindResponses, nil, nil, 0)
	req.SetState(BPSPluginID, &bpsRequest{})
	env := &plugins.ReqEnv{Request: req, Account: account, Model: "gpt-6-sol", Body: []byte(`{"model":"gpt-6-sol","input":"hi"}`), CacheKey: "cache"}
	ctx := WithCodexIdentityStore(plugins.WithRequest(context.Background(), req), nil)
	resp, err := bpsPlugin{}.Execute(ctx, env)
	return resp, env, err
}

func bpsOrgAccount(id int64) *auth.Account {
	return withBPSOverride((&auth.Account{DBID: id, AccountID: "org-account", AccessToken: "at"}).SetCodexBPSOptions(auth.CodexBPSAccountOptions{Profile: auth.BPSExcel}), true)
}

func TestBPSOrgRateLimitRetriesTheSameAccount(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit func() (int, string, string)
	}{
		{name: "http 429", limit: func() (int, string, string) { return http.StatusTooManyRequests, "application/json", bpsOrgTPMBody }},
		{name: "stream event", limit: func() (int, string, string) {
			return http.StatusOK, "text/event-stream", bpsOrgSSE(bpsOrgStreamCreated, bpsOrgStreamFailure)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			freshBPSAccountStates(t)
			account := bpsOrgAccount(9501)
			u := &bpsOrgUpstream{respond: func(n int32) (int, string, string) {
				if n <= 2 {
					return tc.limit()
				}
				return http.StatusOK, "text/event-stream", bpsOrgSSE(bpsOrgStreamCreated, bpsOrgStreamComplete)
			}}
			installBPSOrgUpstream(t, account, u)
			started := time.Now()
			resp, env, err := runBPSOrgExecute(t, account)
			require.NoError(t, err)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Contains(t, string(body), "resp_ok")
			require.NotContains(t, string(body), "organization", "the retried stream never reaches the client")
			require.EqualValues(t, 3, u.calls.Load(), "two org limits, then success, on the same account")
			require.Less(t, time.Since(started), 2*time.Second, "waits follow the 29ms hint")
			_, cooling := bpsAccountCooling(context.Background(), nil, account.ID())
			require.False(t, cooling, "an org limit never cools the account")
			meta, _ := env.State(bpsAttemptMetaKey).(map[string]string)
			require.Equal(t, "org", meta["rate_limit_scope"])
			require.Equal(t, "29", meta["rate_limit_hint_ms"])
			require.Equal(t, "2", meta["org_retries"])
		})
	}
}

func TestBPSOrgRateLimitRetriesAreCapped(t *testing.T) {
	freshBPSAccountStates(t)
	account := bpsOrgAccount(9511)
	u := &bpsOrgUpstream{respond: func(int32) (int, string, string) {
		return http.StatusTooManyRequests, "application/json", bpsOrgTPMBody
	}}
	installBPSOrgUpstream(t, account, u)
	resp, env, err := runBPSOrgExecute(t, account)
	require.NoError(t, err)
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	require.EqualValues(t, 1+bpsOrgRetryLimit, u.calls.Load(), "3 retries per request")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Contains(t, string(body), "Rate limit reached", "the final response stays readable")

	// Handed on, it is still not the account's fault.
	bpsPlugin{}.FilterHeaders(env, http.Header{})
	_, err = bpsPlugin{}.TransformJSON(env, http.StatusTooManyRequests, body)
	require.NoError(t, err)
	_, cooling := bpsAccountCooling(context.Background(), nil, account.ID())
	require.False(t, cooling)
	require.Equal(t, "org", env.State(bpsAttemptMetaKey).(map[string]string)["rate_limit_scope"])
}

func TestBPSPerAccountRateLimitKeepsTheAccountBackoff(t *testing.T) {
	freshBPSAccountStates(t)
	account := bpsOrgAccount(9521)
	u := &bpsOrgUpstream{respond: func(int32) (int, string, string) {
		return http.StatusTooManyRequests, "application/json", bpsAccountRateBody
	}}
	installBPSOrgUpstream(t, account, u)
	resp, env, err := runBPSOrgExecute(t, account)
	require.NoError(t, err)
	require.EqualValues(t, 1, u.calls.Load(), "no same-account retry")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	bpsPlugin{}.FilterHeaders(env, http.Header{})
	_, err = bpsPlugin{}.TransformJSON(env, http.StatusTooManyRequests, body)
	require.NoError(t, err)
	record, cooling := bpsAccountCooling(context.Background(), nil, account.ID())
	require.True(t, cooling)
	require.Equal(t, BPSRateLimitedReason, record.Reason)
	require.Equal(t, "account", env.State(bpsAttemptMetaKey).(map[string]string)["rate_limit_scope"])
}

func TestBPSOrgRateLimitMidStreamDoesNotCoolTheAccount(t *testing.T) {
	freshBPSAccountStates(t)
	account := withBPSOverride(&auth.Account{DBID: 9531, AccountID: "mid-stream", AccessToken: "at"}, true)
	env, _, ctx := streamFailureEnv(t, bpsRoutingHandler(t, account), account, `{"model":"gpt-6-sol","input":"hi"}`)
	_, err := bpsPlugin{}.TransformSSEFrame(env, "response.failed", []byte(bpsOrgStreamFailure))
	require.NoError(t, err)
	_, cooling := bpsAccountCooling(ctx, nil, account.ID())
	require.False(t, cooling)
	meta, _ := env.State(bpsAttemptMetaKey).(map[string]string)
	require.Equal(t, "org", meta["rate_limit_scope"])
	require.Equal(t, "29", meta["rate_limit_hint_ms"])
}

func TestBPSOrgModelPauseSpreadsBursts(t *testing.T) {
	p := &bpsOrgModelPause{}
	now := time.Now()
	require.Equal(t, 29*time.Millisecond, p.observe("gpt-6-sol", 29*time.Millisecond, now))
	// A second org limit within a second pauses the model for its hint.
	require.Equal(t, 800*time.Millisecond, p.observe("gpt-6-sol", 800*time.Millisecond, now.Add(100*time.Millisecond)))
	require.Equal(t, 750*time.Millisecond, p.observe("gpt-6-sol", 10*time.Millisecond, now.Add(150*time.Millisecond)), "later attempts wait out the pause")
	require.Equal(t, 10*time.Millisecond, p.observe("gpt-5.6-sol", 10*time.Millisecond, now), "per model")
	require.Equal(t, bpsOrgRetryMaxWait, p.observe("gpt-6-luna", 30*time.Second, now), "capped")
	require.LessOrEqual(t, bpsOrgJitter(4900*time.Millisecond), bpsOrgRetryMaxWait)
}
