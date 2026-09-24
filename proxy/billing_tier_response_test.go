package proxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestBillingTierResponseProjection(t *testing.T) {
	for _, tc := range []struct{ name, requested, response, tier, source string }{
		{"actual priority", "", `{"object":"response","service_tier":"priority","output":[]}`, "priority", "upstream_response"},
		{"actual downgrade", "priority", `{"object":"response","service_tier":"default","output":[]}`, "default", "upstream_response"},
		{"request fallback", "priority", `{"object":"response","output":[]}`, "priority", "effective_request"},
		{"unknown", "", `{"object":"response","output":[]}`, "", "unobserved"},
		{"compact", "priority", `{"object":"response.compaction","output":[]}`, "priority", "effective_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.WithValue(t.Context(), billingTierRequestKey{}, tc.requested)
			out := projectBillingTierResponse(ctx, []byte(tc.response))
			d := gjson.GetBytes(out, "codex2api_billing")
			require.EqualValues(t, 1, d.Get("version").Int())
			require.Equal(t, tc.tier, d.Get("service_tier").String())
			require.Equal(t, tc.source, d.Get("source").String())
			require.Equal(t, tc.requested, d.Get("requested_service_tier").String())
			require.Equal(t, gjson.Get(tc.response, "service_tier").Raw, gjson.GetBytes(out, "service_tier").Raw)
			require.Equal(t, []byte(tc.response), projectBillingTierResponse(t.Context(), []byte(tc.response)))
		})
	}
}

func TestBillingTierSurvivesJSONAndSSEPrivacyBoundary(t *testing.T) {
	ctx := context.WithValue(t.Context(), billingTierRequestKey{}, "")
	response := `{"object":"response","status":"completed","service_tier":"priority","output":[],"usage":{"input_tokens":17,"output_tokens":3}}`
	for _, sse := range []bool{false, true} {
		body, contentType := response, "application/json"
		if sse {
			body = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + response + "}\n\ndata: [DONE]\n\n"
			contentType = "text/event-stream"
		}
		resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body))}
		require.NoError(t, maskTurnStateResponseMode(ctx, &auth.Account{DBID: 89013}, resp, true))
		out, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		payload := string(out)
		path := "codex2api_billing"
		if sse {
			for _, line := range strings.Split(payload, "\n") {
				if strings.HasPrefix(line, "data: {") {
					payload = strings.TrimPrefix(line, "data: ")
					break
				}
			}
			path = "response." + path
		}
		require.Equal(t, "priority", gjson.Get(payload, path+".service_tier").String(), payload)
		require.Equal(t, "upstream_response", gjson.Get(payload, path+".source").String())
		require.Contains(t, payload, `"input_tokens":17`)
	}
}

func TestClientServiceTierDiagnosticRetainsOriginalValue(t *testing.T) {
	for _, tc := range []struct {
		body, state, value string
		hasValue, redacted bool
	}{
		{`{}`, "absent", "", false, false},
		{`{"service_tier":null}`, "null", "", false, false},
		{`{"service_tier":" Priority "}`, "string", " Priority ", true, false},
		{`{"service_tier":[]}`, "array", "", false, false},
		{`{"service_tier":"private-arbitrary-content"}`, "string", "", false, true},
	} {
		d := captureClientServiceTier([]byte(tc.body))
		require.Equal(t, tc.state, d.State)
		require.Equal(t, tc.redacted, d.Redacted)
		if tc.hasValue {
			require.NotNil(t, d.Value)
			require.Equal(t, tc.value, *d.Value)
		} else {
			require.Nil(t, d.Value)
		}
	}
}
