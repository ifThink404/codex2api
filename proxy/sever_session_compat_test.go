package proxy

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestSeverAccountIdentityMapsNonV7Sessions(t *testing.T) {
	t.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	for _, source := range []string{"75767386-0587-4ccb-a6c6-2c4fd366eb45", "SDK-1"} {
		t.Run(source, func(t *testing.T) {
			db, err := database.New("sqlite", filepath.Join(t.TempDir(), "identity.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			account := &auth.Account{DBID: 1695, AccountID: accountIdentitySampleAccount}
			headers := http.Header{"Session_id": []string{source}}
			body := []byte(`{"model":"gpt-5.6-sol","input":"1","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"client_metadata":{"session_id":"` + source + `","context_window_id":"` + source + `"}}`)
			ctx := WithCodexIdentityStore(context.Background(), db)
			first := NewCodexTransportFingerprint(account, headers, body, "cache")
			require.NoError(t, first.ClaimSessionIdentity(ctx, account, "test-user-key"))
			mapped := first.ApplyBody(body)
			outbound := gjson.GetBytes(mapped, "client_metadata.session_id").String()
			parsed, err := uuid.Parse(outbound)
			require.NoError(t, err)
			require.Equal(t, uuid.Version(7), parsed.Version())
			require.NotEqual(t, source, outbound)
			require.Equal(t, outbound, first.headers.Get(codexSessionIDHeader))
			require.Equal(t, outbound, gjson.GetBytes(mapped, "client_metadata.context_window_id").String())
			for _, field := range []string{"input", "tools"} {
				require.Equal(t, gjson.GetBytes(body, field).Raw, gjson.GetBytes(mapped, field).Raw)
			}
			resumed := NewCodexTransportFingerprint(account, headers, body, "cache")
			require.NoError(t, resumed.ClaimSessionIdentity(ctx, account, "test-user-key"))
			require.Equal(t, mapped, resumed.ApplyBody(body))
			other := &auth.Account{DBID: 1696, AccountID: "761373c1-f1a9-4ca9-8682-a0594b30c36c"}
			different := NewCodexTransportFingerprint(other, headers, body, "cache")
			require.NoError(t, different.ClaimSessionIdentity(ctx, other, "test-user-key"))
			require.NotEqual(t, outbound, gjson.GetBytes(different.ApplyBody(body), "client_metadata.session_id").String())
			if source != strings.ToLower(source) {
				lowerHeaders := http.Header{}
				lowerHeaders.Set(codexSessionIDHeader, strings.ToLower(source))
				lowerBody := []byte(strings.ReplaceAll(string(body), source, strings.ToLower(source)))
				lower := NewCodexTransportFingerprint(account, lowerHeaders, lowerBody, "cache")
				require.NoError(t, lower.ClaimSessionIdentity(ctx, account, "test-user-key"))
				require.NotEqual(t, outbound, gjson.GetBytes(lower.ApplyBody(lowerBody), "client_metadata.session_id").String(), "opaque SDK IDs remain case-sensitive")
			}
		})
	}
}

func TestSeverUnboundCompactionStartsAtCurrentWindow(t *testing.T) {
	for _, mode := range []string{"off", "observe", "enforce"} {
		t.Run(mode, func(t *testing.T) {
			handler := newWindowAuthorizationHandler(t)
			config := handler.store.GetPromptFilterConfig()
			config.Advanced.Risk.SessionContinuityMode = mode
			handler.store.SetPromptFilterConfig(config)
			account := &auth.Account{DBID: 1707, AccessToken: "dummy", Status: auth.StatusReady}
			handler.store.AddAccount(account)
			request, body := continuityTestRequest(15, "compaction")
			body = []byte(strings.Replace(string(body), `"model":`, `"input":[{"role":"user","content":"keep full history"}],"model":`, 1))
			key := "unbound-compaction::api-key:101"
			require.Nil(t, handler.prepareSessionContinuity(request, requestSessionIdentity{stableIdentity: true}, key, body))
			require.Zero(t, selectionTraceForRequest(request).PinnedAccount())
			require.Empty(t, continuityRequest(request).RestartReason, "do not discard compaction input as a lossy restart")
			require.Nil(t, handler.commitSessionContinuity(request, account))
			stored, found, err := handler.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, account.ID(), stored.AccountID)
			require.True(t, stored.NumberKnown)
			require.Equal(t, uint64(15), stored.Number)
			out, _, err := PrepareSessionRestartOutbound(request.Request.Context(), account, body, nil)
			require.NoError(t, err)
			require.Equal(t, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(out, "input").Raw)
		})
	}
}
