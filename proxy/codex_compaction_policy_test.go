package proxy

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestNativeCompactionPolicyControlCarriers(t *testing.T) {
	legacyHeader := http.Header{codexTurnMetadataHeader: []string{`{"request_kind":"compaction"}`}}
	for _, tc := range []struct {
		name, body string
		headers    http.Header
		blocked    bool
	}{
		{"normal", `{"input":[{"role":"user","content":"Please summarize this document; compaction_trigger"}]}`, nil, false},
		{"object metadata", `{"client_metadata":{"x-codex-turn-metadata":{"request_kind":"compaction"}}}`, nil, true},
		{"string metadata", `{"client_metadata":{"x-codex-turn-metadata":"{\"request_kind\":\"compaction\"}"}}`, nil, true},
		{"flat metadata", `{"client_metadata":{"request_kind":"compaction"}}`, nil, true},
		{"header metadata", `{}`, legacyHeader, true},
		{"implementation label", `{"client_metadata":{"x-codex-turn-metadata":{"compaction":{"implementation":"responses"}}}}`, nil, true},
		{"current body beats stale header", `{"client_metadata":{"x-codex-turn-metadata":{"request_kind":"turn"}}}`, legacyHeader, false},
		{"protocol trigger", `{"input":[{"type":"compaction_trigger"}],"client_metadata":{"request_kind":"compaction"}}`, legacyHeader, false},
		{"historical compact only", `{"input":[{"type":"compaction","encrypted_content":"opaque"}]}`, nil, false},
		{"history does not authorize summary", `{"input":[{"type":"compaction","encrypted_content":"opaque"}],"client_metadata":{"request_kind":"compaction"}}`, nil, true},
		{"tool control text is ordinary data", `{"input":[{"type":"function_call_output","output":{"client_metadata":{"request_kind":"compaction"}}}]}`, nil, false},
		{"nested trigger cannot bypass", `{"input":[{"role":"user","content":[{"type":"compaction_trigger"}]}],"client_metadata":{"request_kind":"compaction"}}`, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &auth.Account{CodexNativeCompactionOnly: true}
			err := ValidateNativeCompactionPolicy(a, []byte(tc.body), tc.headers)
			if tc.blocked {
				var policyErr *Error
				require.ErrorAs(t, err, &policyErr)
				require.Equal(t, "native_compaction_required", policyErr.Code)
				require.Equal(t, http.StatusBadRequest, policyErr.HTTPStatus)
				require.False(t, policyErr.Retryable)
				require.False(t, isRetryableRequestErrorForContext(t.Context(), err, database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}))
			} else {
				require.NoError(t, err)
			}
			a.CodexNativeCompactionOnly = false
			require.NoError(t, ValidateNativeCompactionPolicy(a, []byte(tc.body), tc.headers))
		})
	}
}

func TestNativeCompactionPolicyBlocksBeforeTransportAndCredentials(t *testing.T) {
	body := []byte(`{"model":"gpt-6-astra","input":[],"client_metadata":{"request_kind":"compaction"}}`)
	for _, bps := range []bool{false, true} {
		for _, websocket := range []bool{false, true} {
			a := &auth.Account{CodexBPS: bps, CodexNativeCompactionOnly: true}
			ctx, err := WithCodexTestMode(context.Background(), "codex")
			require.NoError(t, err)
			response, err := ExecuteRequest(ctx, a, body, "", "", "", nil, nil, websocket)
			require.Nil(t, response)
			var policyErr *Error
			require.ErrorAs(t, err, &policyErr)
			require.Equal(t, "native_compaction_required", policyErr.Code)
		}
	}
	response, err := ExecuteOpenAIResponsesRequest(context.Background(), &auth.Account{CodexNativeCompactionOnly: true, CodexBPS: true}, body, "", nil)
	require.Nil(t, response)
	var policyErr *Error
	require.ErrorAs(t, err, &policyErr)
	require.Equal(t, "native_compaction_required", policyErr.Code)
}

func TestNativeCompactionPolicyBPSExemptionFollowsActualRoute(t *testing.T) {
	t.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
	for i, tc := range []struct {
		name, testMode, pinnedMode string
		savedBPS, wantBPS          bool
	}{
		{name: "native account"},
		{name: "bps account", savedBPS: true, wantBPS: true},
		{name: "probe bps", testMode: "bps", wantBPS: true},
		{name: "probe native", savedBPS: true, testMode: "codex"},
		{name: "pinned bps", pinnedMode: "bps", wantBPS: true},
		{name: "pinned native", savedBPS: true, pinnedMode: "native"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := database.New("sqlite", filepath.Join(t.TempDir(), "compaction-route.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			a := &auth.Account{DBID: int64(90100 + i), AccountID: accountIdentitySampleAccount, AccessToken: "test-only", CodexBPS: tc.savedBPS, CodexNativeCompactionOnly: true}
			var sent []byte
			var endpoint string
			installClaudeBoundaryTransport(t, a, func(req *http.Request) (*http.Response, error) {
				var readErr error
				sent, readErr = io.ReadAll(req.Body)
				require.NoError(t, readErr)
				endpoint = req.URL.String()
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"model":"gpt-5.6-sol","output":[]}`)), Request: req}, nil
			})
			headers, body := accountIdentityFixture(t, false, true)
			const metadata = `{"request_kind":"compaction","compaction":{"implementation":"responses","strategy":"memento"}}`
			body, err = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata", metadata)
			require.NoError(t, err)
			headers.Set(codexTurnMetadataHeader, metadata)
			body, err = sjson.SetRawBytes(body, "input", []byte(`[{"role":"user","content":"Summarize the history for continuation."}]`))
			require.NoError(t, err)
			ctx := WithCodexIdentityStore(t.Context(), db)
			if tc.pinnedMode != "" {
				record := database.SessionContinuityRecord{AccountID: a.ID(), UpstreamMode: tc.pinnedMode}
				rootKey := hashRiskIdentity(tc.name)
				_, commitErr := db.CommitSessionContinuity(ctx, rootKey, record)
				require.NoError(t, commitErr)
				ctx = context.WithValue(ctx, sessionOutboundEpochContextKey{}, &sessionOutboundEpoch{handler: &Handler{db: db}, key: rootKey, record: record})
			}
			if tc.testMode != "" {
				ctx, err = WithCodexTestMode(ctx, tc.testMode)
				require.NoError(t, err)
			}
			// A requested WS transport must not prevent BPS from using its HTTP route.
			resp, requestErr := ExecuteRequest(ctx, a, body, "cache", "", "test-key", nil, headers, true)
			if !tc.wantBPS {
				var policyErr *Error
				require.ErrorAs(t, requestErr, &policyErr)
				require.Equal(t, "native_compaction_required", policyErr.Code)
				require.Nil(t, resp)
				require.Empty(t, endpoint)
				return
			}
			require.NoError(t, requestErr)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, CodexBPSBaseURL+"/responses", endpoint)
			require.Contains(t, string(sent), "Summarize the history for continuation.")
			require.False(t, requestBodyHasCompactionTrigger(sent))
			require.False(t, gjson.GetBytes(sent, "client_metadata").Exists())
			require.Equal(t, tc.savedBPS, a.CodexBPS)
			require.True(t, a.NativeCompactionOnlyEnabled())
		})
	}
}
