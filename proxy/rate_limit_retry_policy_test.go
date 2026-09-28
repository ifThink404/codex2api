package proxy

import (
	"net/http"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
)

func TestRateLimitRetryPolicyIndependentAcrossFailureShapes(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	h := &Handler{store: auth.NewStore(nil, nil, nil)}
	defer h.store.Stop()
	body := []byte(`{"error":{"type":"rate_limit_error","message":"temporarily limited"}}`)
	upload := bpsAttachmentFailure(t.Context(), nil, "", "http", 429, nil, body, nil)
	outcome := streamOutcome{penalize: true, logStatusCode: 429, failureKind: "rate_limited", failurePayload: body}
	for _, transport := range []string{"rotate", "sticky"} {
		h.store.SetTransportRetryPolicy(transport)
		for _, rate := range []string{"off", "sticky", "rotate"} {
			UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.RateLimitRetryPolicy = rate; return s })
			for _, continuous := range []database.ContinuousRetryPolicy{{}, {Enabled: true, CatchAll: true}} {
				t.Run(transport+"_"+rate+"_"+database.EncodeContinuousRetryPolicy(continuous), func(t *testing.T) {
					general, limited := 0, 0
					want := rate != "off"
					require.Equal(t, want, shouldRetryHTTPStatus(429, body, &general, &limited, 2, 2, continuous))
					d := h.httpSessionFailureDispositionForPolicy(429, body, want, continuous)
					require.Equal(t, rate == "sticky", d.retrySameAccount)
					require.Equal(t, rate != "rotate", d.retainAffinity)
					require.Equal(t, want, shouldTransparentRetryStreamWithBudgets(outcome, &general, &limited, 2, 2, false, nil, nil, continuous))
					d = h.streamSessionFailureDispositionForPolicy(outcome, body, want, continuous)
					require.Equal(t, rate == "sticky", d.retrySameAccount)
					counter, limit := requestErrorRetryBudget(upload, &general, &limited, 4, 4)
					require.Equal(t, want, shouldRetryRequestError(upload, counter, limit, continuous))
					require.Equal(t, rate == "sticky", h.shouldStickyTransportRetry(upload, "bps_attachment_upload", false, want, continuous))
					require.Zero(t, general, "HTTP, stream and upload 429 must not consume the general counter")
					server := h.httpSessionFailureDisposition(503, nil, true)
					require.Equal(t, transport == "sticky", server.retrySameAccount, "429 policy cannot alter 5xx")
				})
			}
		}
	}
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.RateLimitRetryPolicy = "sticky"; return s })
	d := h.httpSessionFailureDisposition(429, []byte(`{"error":{"type":"usage_limit_reached"}}`), true)
	require.False(t, d.retrySameAccount, "an exhausted account cannot be forced back into service")
	require.True(t, d.permanentAccount)
	general, limited := 0, 0
	require.False(t, shouldRetryHTTPStatus(http.StatusTooManyRequests, []byte(`{"error":{"code":"cyber_policy"}}`), &general, &limited, 2, 2, database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}))
}
