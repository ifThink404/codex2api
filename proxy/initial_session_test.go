package proxy

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
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

func TestInitialSessionAgeBoundaries(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	for _, sample := range []struct {
		age  time.Duration
		want string
	}{
		{0, "allowed"},
		{time.Minute, "allowed"},
		{time.Minute + time.Millisecond, "expired"},
		{-time.Millisecond, "allowed"},
		{-time.Minute, "allowed"},
		{-time.Minute - time.Millisecond, "future"},
	} {
		t.Run(sample.age.String(), func(t *testing.T) {
			d := evaluateInitialSessionAge(initialTestID(now.Add(-sample.age)), now, 60)
			require.Equal(t, sample.want, d.Result)
			require.Equal(t, sample.age.Milliseconds(), d.AgeMillis)
		})
	}
	for _, id := range []string{"", "invalid", uuid.NewString(), "00000000-0000-7000-0000-000000000000"} {
		require.Equal(t, "invalid", evaluateInitialSessionAge(id, now, 60).Result)
	}
	require.Equal(t, "expired", evaluateInitialSessionAge(initialTestID(now.Add(-2*time.Second)), now, 1).Result)
	encoded, err := json.Marshal(initialSessionAdmissionError())
	require.NoError(t, err)
	for _, secret := range []string{"age_ms", "expired", "60", "uuid", "reason"} {
		require.NotContains(t, string(encoded), secret)
	}
}

func TestInitialSessionAgeToggleRetainsIdentityAndOutboundCleanup(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	now := time.Now().UTC()
	for _, disabled := range []bool{true, false} {
		ApplyRuntimeSettingsFromSystem(&database.SystemSettings{CodexInitialSessionMaxAgeSeconds: 12, CodexInitialSessionAgeCheckDisabled: disabled})
		status := GetInitialSessionAgeStatus()
		require.Equal(t, !disabled, status.Enabled)
		require.Equal(t, 12, status.LimitSeconds)
		for _, age := range []time.Duration{time.Hour, -time.Hour} {
			request, _ := continuityTestRequest(0, "turn")
			usageRequestDiagnosticState(request).StartedAt = now
			err := checkInitialSessionAdmission(request, initialTestID(now.Add(-age)))
			if !disabled {
				require.NotNil(t, err)
				continue
			}
			require.Nil(t, err)
			d := usageRequestDiagnosticState(request).InitialSession
			require.True(t, d.AgeCheckDisabled)
			require.Equal(t, age.Milliseconds(), d.AgeMillis)
			out, headers, outboundErr := PrepareInitialSessionOutbound(request.Request.Context(), &auth.Account{DBID: 1}, []byte(`{"input":"hi","client_metadata":{"x-codex-turn-state":"stale"}}`), http.Header{"X-Codex-Turn-State": []string{"stale"}})
			require.NoError(t, outboundErr)
			require.Empty(t, headers.Get("X-Codex-Turn-State"))
			require.False(t, gjson.GetBytes(out, "client_metadata.x-codex-turn-state").Exists())
			before := GetInitialSessionAgeStatus().SinceStart.Samples
			require.Nil(t, checkInitialSessionAdmission(request, initialTestID(now.Add(-age))))
			require.Equal(t, before, GetInitialSessionAgeStatus().SinceStart.Samples)
		}
		request, _ := continuityTestRequest(0, "turn")
		require.NotNil(t, checkInitialSessionAdmission(request, "invalid-id"), "disabling age validation must retain ID format validation")
	}
}

func TestInitialSessionClockSkewUsesConfiguredLimit(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	now := time.Date(2026, 9, 19, 6, 21, 7, 343_000_000, time.UTC)
	for _, sample := range []struct {
		name  string
		id    string
		limit int
		want  string
	}{
		{"reported_future_within_default", "01a0b854-0a0d-72a1-ac01-b1deed77eb4d", 60, "allowed"},
		{"reported_future_outside_custom", "01a0b854-0a0d-72a1-ac01-b1deed77eb4d", 30, "future"},
		{"reported_old_outside_default", "01a0b84e-0d83-7ba1-9204-37e4ff875834", 60, "expired"},
		{"custom_positive_boundary", initialTestID(now.Add(-time.Second)), 1, "allowed"},
		{"custom_negative_boundary", initialTestID(now.Add(time.Second)), 1, "allowed"},
		{"custom_positive_outside", initialTestID(now.Add(-time.Second - time.Millisecond)), 1, "expired"},
		{"custom_negative_outside", initialTestID(now.Add(time.Second + time.Millisecond)), 1, "future"},
	} {
		t.Run(sample.name, func(t *testing.T) {
			settings := previous
			settings.CodexInitialSessionMaxAgeSeconds = sample.limit
			ApplyRuntimeSettings(settings)
			request, _ := continuityTestRequest(0, "turn")
			usageRequestDiagnosticState(request).StartedAt = now
			err := checkInitialSessionAdmission(request, sample.id)
			diagnostic := usageRequestDiagnosticState(request).InitialSession
			require.Equal(t, sample.want, diagnostic.Result)
			require.Equal(t, sample.limit, diagnostic.LimitSeconds)
			if sample.want == "allowed" {
				require.Nil(t, err)
				require.Same(t, diagnostic, request.Request.Context().Value(initialSessionContextKey{}))
			} else {
				require.NotNil(t, err)
				require.Equal(t, "codex_session_identity_unavailable", string(err.Code))
			}
		})
	}
}

func TestInitialSessionStatsBucketsAndConcurrency(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	s := initialAgeStats{started: now}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.record(initialSessionDiagnostic{Result: "allowed", ReceivedAt: now, AgeMillis: 100})
		}()
	}
	wg.Wait()
	s.record(initialSessionDiagnostic{Result: "allowed", ReceivedAt: now, AgeMillis: -43000})
	s.record(initialSessionDiagnostic{Result: "expired", ReceivedAt: now, AgeMillis: 60100})
	s.record(initialSessionDiagnostic{Result: "future", ReceivedAt: now, AgeMillis: -61000})
	s.record(initialSessionDiagnostic{Result: "invalid", ReceivedAt: now})
	x := s.snapshot(now, 60)
	require.Equal(t, uint64(104), x.RecentHour.Samples)
	require.Equal(t, uint64(103), x.RecentHour.ValidSamples)
	require.Equal(t, uint64(101), x.RecentHour.Allowed)
	require.Equal(t, uint64(1), x.RecentHour.Rejected)
	require.Equal(t, uint64(1), x.RecentHour.Future)
	require.Equal(t, uint64(1), x.RecentHour.Invalid)
	require.Equal(t, int64(61000), x.RecentHour.MaxMillis)
	require.InDelta(t, 174100.0/103, x.RecentHour.AverageMillis, 0.0001)
	require.Equal(t, x.RecentHour, x.SinceStart)
	require.Equal(t, uint64(0), s.snapshot(now.Add(time.Hour), 60).RecentHour.Samples)
	s.record(initialSessionDiagnostic{Result: "allowed", ReceivedAt: now.Add(time.Hour), AgeMillis: 20})
	x = s.snapshot(now.Add(time.Hour), 60)
	require.Equal(t, uint64(1), x.RecentHour.Samples)
	require.Equal(t, uint64(105), x.SinceStart.Samples)
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
			require.Equal(t, "allowed", state.InitialSession.Result)
			require.Equal(t, int64(-43000), state.InitialSession.AgeMillis)
			// A delayed second preparation keeps the ingress timestamp and counts once.
			before := GetInitialSessionAgeStatus().SinceStart.Samples
			require.Nil(t, h.prepareSessionContinuity(r, identity, "fresh", body))
			require.Equal(t, before, GetInitialSessionAgeStatus().SinceStart.Samples)
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
			require.NotNil(t, h.prepareSessionContinuity(r3, identity, "unbound", body))
			require.Equal(t, "expired", usageRequestDiagnosticState(r3).InitialSession.Result)
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
	require.Nil(t, checkInitialSessionAdmission(r, initialTestID(now)))
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
