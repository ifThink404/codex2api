package proxy

import (
	"context"
	"encoding/binary"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func initialTestID(at time.Time) string {
	u := uuid.New()
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(at.UnixMilli()))
	copy(u[:6], b[2:])
	u[6] = (u[6] & 15) | 0x70
	return u.String()
}

func TestInitialSessionDisabledKeepsOutboundCleanup(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	for _, disabled := range []bool{true, false} {
		ApplyRuntimeSettingsFromSystem(&database.SystemSettings{CodexInitialSessionMaxAgeSeconds: 12, CodexInitialSessionAgeCheckDisabled: disabled})
		require.False(t, GetInitialSessionAgeStatus().Enabled, "legacy settings cannot restore admission checks on sever")
		request, _ := continuityTestRequest(0, "turn")
		prepareInitialSession(request)
		diagnostic := usageRequestDiagnosticState(request).InitialSession
		require.True(t, diagnostic.AgeCheckDisabled)
		require.Equal(t, "disabled", diagnostic.Result)
		prepareInitialSession(request)
		require.Same(t, diagnostic, request.Request.Context().Value(initialSessionContextKey{}))
		out, headers, err := PrepareInitialSessionOutbound(request.Request.Context(), &auth.Account{DBID: 1}, []byte(`{"input":"hi","client_metadata":{"x-codex-turn-state":"stale"}}`), http.Header{"X-Codex-Turn-State": []string{"stale"}})
		require.NoError(t, err)
		require.Empty(t, headers.Get("X-Codex-Turn-State"))
		require.False(t, gjson.GetBytes(out, "client_metadata.x-codex-turn-state").Exists())
		require.Equal(t, "hi", gjson.GetBytes(out, "input").String())
	}
}

func TestInitialSessionAdmissionModesBindingsAndCleanup(t *testing.T) {
	for _, mode := range []string{"off", "observe", "enforce"} {
		t.Run(mode, func(t *testing.T) {
			h := newWindowAuthorizationHandler(t)
			cfg := h.store.GetPromptFilterConfig()
			cfg.Advanced.Risk.SessionContinuityMode = mode
			h.store.SetPromptFilterConfig(cfg)
			now := time.Now().UTC()
			id := initialTestID(now.Add(43 * time.Second))
			r, body := continuityTestRequest(0, "turn")
			body = []byte(strings.ReplaceAll(string(body), continuityTestThread, id))
			state := usageRequestDiagnosticState(r)
			state.StartedAt = now
			identity := requestSessionIdentity{stableIdentity: true}
			require.Nil(t, h.prepareSessionContinuity(r, identity, "fresh", body))
			require.Equal(t, "disabled", state.InitialSession.Result)
			// Repeated preparation retains the same cleanup marker.
			before := state.InitialSession
			require.Nil(t, h.prepareSessionContinuity(r, identity, "fresh", body))
			require.Same(t, before, state.InitialSession)
			a := &auth.Account{DBID: 99, AccessToken: "dummy", Status: auth.StatusReady}
			h.store.AddAccount(a)
			payload := []byte(`{"input":[{"type":"reasoning","encrypted_content":"opaque"}],"tools":[{"type":"function","name":"exec"}],"client_metadata":{"x-codex-turn-state":"old","session_id":"unchanged"}}`)
			headers := http.Header{"X-Codex-Turn-State": []string{"old"}}
			out, hdr, err := PrepareSessionRestartOutbound(r.Request.Context(), a, payload, headers)
			require.NoError(t, err)
			require.Empty(t, hdr.Get("X-Codex-Turn-State"))
			require.False(t, gjson.GetBytes(out, "client_metadata.x-codex-turn-state").Exists())
			require.Equal(t, gjson.GetBytes(payload, "input").Raw, gjson.GetBytes(out, "input").Raw)
			require.Equal(t, gjson.GetBytes(payload, "tools").Raw, gjson.GetBytes(out, "tools").Raw)
			require.Equal(t, "old", headers.Get("X-Codex-Turn-State"))
			require.True(t, state.InitialSession.HeaderStateRemoved)
			require.True(t, state.InitialSession.BodyStateRemoved)
			// New-account retry and callers without verified state both strip it.
			_, hdr, err = PrepareSessionRestartOutbound(r.Request.Context(), &auth.Account{DBID: 100}, payload, headers)
			require.NoError(t, err)
			require.Empty(t, hdr.Get("X-Codex-Turn-State"))
			out, hdr, err = PrepareSessionRestartOutbound(context.Background(), a, payload, headers)
			require.NoError(t, err)
			require.False(t, gjson.GetBytes(out, "client_metadata.x-codex-turn-state").Exists())
			require.Empty(t, hdr.Get("X-Codex-Turn-State"))
			require.Equal(t, gjson.GetBytes(payload, "input").Raw, gjson.GetBytes(out, "input").Raw)
			require.Nil(t, h.commitSessionContinuity(r, a))
			r2, _ := continuityTestRequest(0, "turn")
			usageRequestDiagnosticState(r2).StartedAt = now.Add(time.Hour)
			require.Nil(t, h.prepareSessionContinuity(r2, identity, "fresh", body))
			require.Nil(t, usageRequestDiagnosticState(r2).InitialSession)
			r3, _ := continuityTestRequest(0, "turn")
			usageRequestDiagnosticState(r3).StartedAt = now.Add(time.Hour)
			require.Nil(t, h.prepareSessionContinuity(r3, identity, "unbound", body))
			require.Equal(t, "disabled", usageRequestDiagnosticState(r3).InitialSession.Result)
		})
	}
}

func TestInitialSessionHTTPOutboundStripsStateAndFrameReset(t *testing.T) {
	previous := GetResinConfig()
	SetResinConfig(nil)
	t.Cleanup(func() { SetResinConfig(previous) })
	now := time.Now().UTC()
	r, _ := continuityTestRequest(0, "turn")
	usageRequestDiagnosticState(r).StartedAt = now
	prepareInitialSession(r)
	account := &auth.Account{DBID: 998813, AccessToken: "dummy", CodexFingerprintMode: auth.CodexFingerprintModeOff}
	key := clientPoolKey(account, "", codexTransportModeFromEnv())
	var received []byte
	entry := &poolEntry{client: &http.Client{Transport: environmentTestTransport{&received}}}
	entry.touch()
	clientPool.Store(key, entry)
	t.Cleanup(func() { clientPool.Delete(key) })
	body := []byte(`{"model":"gpt-5.6-sol","input":"hello","client_metadata":{"x-codex-turn-state":"old"}}`)
	resp, err := ExecuteRequest(r.Request.Context(), account, body, "initial-test", "", "test-key", nil, http.Header{"X-Codex-Turn-State": []string{"old"}}, false)
	require.NoError(t, err)
	resp.Body.Close()
	require.False(t, gjson.GetBytes(received, "client_metadata.x-codex-turn-state").Exists())
	// A new WS frame gets its own admission decision; the previous marker must
	// not strip the newly bound account's state on subsequent requests.
	r.Set(usageRequestDiagnosticsContextKey, (*usageRequestDiagnostics)(nil))
	captureUsageRequestIngress(r, body)
	out, hdr, err := PrepareInitialSessionOutbound(r.Request.Context(), account, body, http.Header{"X-Codex-Turn-State": []string{"new-account-state"}})
	require.NoError(t, err)
	require.Equal(t, body, out)
	require.Equal(t, "new-account-state", hdr.Get("X-Codex-Turn-State"))
}
