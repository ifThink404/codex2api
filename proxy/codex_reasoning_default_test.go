package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestDefaultCodexReasoningPreservesExplicitControls(t *testing.T) {
	for _, body := range []string{`{}`, `{"reasoning":null}`, `{"reasoning":{"summary":"auto"}}`, `{"reasoning":{"effort":null}}`, `{"reasoning_effort":" "}`, `{"reasoning":{"effort":""}}`} {
		out := defaultCodexReasoning([]byte(body))
		require.Equal(t, "low", gjson.GetBytes(out, "reasoning.effort").String(), body)
		if strings.Contains(body, "summary") {
			require.Equal(t, "auto", gjson.GetBytes(out, "reasoning.summary").String())
		}
	}
	for _, body := range []string{`{"reasoning":{"effort":"none"}}`, `{"reasoning":{"effort":"high"}}`, `{"reasoning_effort":"medium"}`, `{"reasoning":{"effort":123}}`, `{"reasoning":"invalid"}`} {
		require.Equal(t, body, string(defaultCodexReasoning([]byte(body))))
	}
}

func TestCodexDefaultReasoningOutboundAndUsage(t *testing.T) {
	t.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "reasoning.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	for i, route := range []string{"native", "word", "excel", "sheets", "powerpoint", "native-ws"} {
		t.Run(route, func(t *testing.T) {
			bps := route != "native" && route != "native-ws"
			a := &auth.Account{DBID: int64(91000 + i), AccountID: accountIdentitySampleAccount, AccessToken: "test-only", CodexBPS: bps, CodexBPSProfile: auth.CodexBPSProfile(route)}
			var sent []byte
			installClaudeBoundaryTransport(t, a, func(req *http.Request) (*http.Response, error) {
				var readErr error
				sent, readErr = io.ReadAll(req.Body)
				require.NoError(t, readErr)
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"model":"gpt-5.5","output":[]}`)), Request: req}, nil
			})
			if route == "native-ws" {
				previous := WebsocketExecuteFunc
				t.Cleanup(func() { WebsocketExecuteFunc = previous })
				WebsocketExecuteFunc = func(ctx context.Context, _ *auth.Account, body []byte, _, _, _ string, _ *DeviceProfileConfig, headers http.Header, _ string) (*http.Response, error) {
					sent = append([]byte(nil), body...)
					UpstreamTransportObserver(ctx).ResponsesInput(body, headers, "")
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"output":[]}`))}, nil
				}
			}
			for _, effort := range []string{"", "high", "none"} {
				headers, body := accountIdentityFixture(t, false, true)
				body, _ = sjson.SetBytes(body, "model", "gpt-5.5")
				body, _ = sjson.DeleteBytes(body, "reasoning")
				body, _ = sjson.DeleteBytes(body, "reasoning_effort")
				if effort != "" {
					body, _ = sjson.SetBytes(body, "reasoning.effort", effort)
				}
				original := string(body)
				ctx := ensureTransportTrace(WithCodexIdentityStore(t.Context(), db))
				resp, err := ExecuteRequest(ctx, a, body, "cache", "", "test-key", nil, headers, route == "native-ws")
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
				want := effort
				if want == "" {
					want = "low"
				}
				require.Equal(t, want, extractReasoningEffort(sent))
				require.Equal(t, original, string(body), "do not alter caller input")
				if bps {
					d := CodexBPSResponseDiagnostic(resp)
					require.Equal(t, effort, d.RequestedReasoningEffort)
					require.Equal(t, want, d.SentReasoningEffort)
				}
				snapshot := snapshotUpstreamTrace(ctx)
				require.Equal(t, want, snapshot.Transport.ReasoningEffort)
				entry := &database.UsageLogInput{AccountID: a.ID(), ReasoningEffort: effort}
				snapshot.apply(entry)
				require.Equal(t, want, entry.ReasoningEffort)
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
				entry = &database.UsageLogInput{AccountID: a.ID(), ReasoningEffort: effort}
				populateUpstreamTrace(c, entry)
				require.Equal(t, want, entry.ReasoningEffort)
			}
		})
	}
}

func TestCodexDefaultReasoningChatTranslation(t *testing.T) {
	body, err := TranslateRequest([]byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}]}`))
	require.NoError(t, err)
	require.Equal(t, "low", extractReasoningEffort(defaultCodexReasoning(body)))
	require.Equal(t, "auto", gjson.GetBytes(defaultCodexReasoning(body), "reasoning.summary").String())
}
