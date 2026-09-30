package proxy

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// A dual-route account (native explicitly on) whose BPS attempt fails before
// any output retries the request on its own native route, as upstream's Excel
// Basispoints adapter did: 5xx, 429, 401, a withdrawn model, unverifiable
// encrypted context and transport failures.
func TestBPSFailuresFallBackToTheSameAccountsNativeRoute(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"usage-policy 403", http.StatusForbidden, bpsPolicyBlockBody},
		{"5xx", http.StatusBadGateway, `{"error":{"message":"upstream failed","type":"server_error"}}`},
		{"429", http.StatusTooManyRequests, `{"error":{"message":"Rate limit exceeded","type":"rate_limit_error"}}`},
		{"model access", http.StatusForbidden, `{"error":{"message":"model access changed","code":"basispoints_model_access_changed"}}`},
		{"encrypted context", http.StatusBadRequest, `{"error":{"message":"cannot verify","code":"invalid_encrypted_content"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := dualRouteFixture(t, func([]byte) (int, string) { return tc.status, tc.body },
				func(body []byte) (int, string) { return 200, nativeSSE(gjson.GetBytes(body, "model").String()) })
			updateBPSConfig(t, func(c BPSConfig) BPSConfig { c.DualRoutePreference = DualRouteBPS; return c })
			recorder := f.serve(t, "/v1/responses", `{"model":"gpt-6-sol","stream":true,"input":"hi"}`)
			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			require.EqualValues(t, 1, f.bps.Load(), "BPS first")
			require.EqualValues(t, 1, f.native.Load(), "then the same account's native route")
			var bpsRow, nativeRow *database.UsageLog
			for _, row := range f.usageRows(t) {
				switch {
				case row.Transport == BPSPluginID:
					bpsRow = row
				case row.Transport == database.TransportNative && row.StatusCode == 200:
					nativeRow = row
				}
			}
			require.NotNil(t, bpsRow)
			require.Equal(t, "same_account", gjson.Get(bpsRow.PluginMeta, "native_retry").String())
			require.NotNil(t, nativeRow)
			require.Equal(t, f.account.ID(), nativeRow.AccountID)
		})
	}
}

func TestBPSTransportFailureFallsBackToTheSameAccountsNativeRoute(t *testing.T) {
	f := dualRouteFixture(t, nil, nil)
	updateBPSConfig(t, func(c BPSConfig) BPSConfig { c.DualRoutePreference = DualRouteBPS; return c })
	installClaudeBoundaryTransport(t, f.account, func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.String(), CodexBPSBaseURL) {
			f.bps.Add(1)
			return nil, errors.New("dial tcp 10.0.0.1:443: connect: connection refused")
		}
		f.native.Add(1)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(nativeSSE("gpt-6-sol"))), Request: r}, nil
	})
	recorder := f.serve(t, "/v1/responses", `{"model":"gpt-6-sol","stream":true,"input":"hi"}`)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.GreaterOrEqual(t, f.bps.Load(), int32(1))
	require.EqualValues(t, 1, f.native.Load())
}

// Without an explicit native route a BPS failure never reaches native, and a
// client error that is not a fallback case stays visible.
func TestBPSFailuresNeverReachNativeWithoutTheExplicitRoute(t *testing.T) {
	f := dualRouteFixture(t, func([]byte) (int, string) {
		return http.StatusBadGateway, `{"error":{"message":"upstream failed","type":"server_error"}}`
	}, func(body []byte) (int, string) { return 200, nativeSSE("gpt-6-sol") })
	f.account.SetCodexBPSOptions(auth.CodexBPSAccountOptions{ImageTrim: true})
	f.serve(t, "/v1/responses", `{"model":"gpt-6-sol","stream":true,"input":"hi"}`)
	require.Zero(t, f.native.Load(), "BPS-owned accounts never spill onto native")

	require.False(t, bpsNativeFallbackFailure("", http.StatusBadRequest, []byte(`{"error":{"message":"bad input"}}`)))
	require.False(t, bpsNativeFallbackFailure("", http.StatusRequestEntityTooLarge, nil))
	require.False(t, bpsNativeFallbackFailure("", http.StatusUnauthorized, nil), "a 401 stays with the core credential handling")
}
