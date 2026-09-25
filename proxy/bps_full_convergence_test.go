package proxy

import (
	"context"
	"fmt"
	"io"
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

func TestBPSFullConvergenceIdentityLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "full.db")
	db, err := database.New("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	account := &auth.Account{DBID: 17, AccountID: accountIdentitySampleAccount, CodexFingerprintMode: auth.CodexFingerprintModeFull}
	body := []byte(`{"model":"gpt-6-astra","input":[{"role":"user","content":"hello"}]}`)
	project := func(owner, session, turn, cache string, profile auth.CodexBPSProfile, epoch *sessionOutboundEpoch, input []byte) *bpsWordIdentityDiagnostic {
		t.Helper()
		ctx := context.WithValue(WithCodexIdentityStore(t.Context(), db), transportUserContextKey{}, owner)
		if epoch != nil {
			ctx = context.WithValue(ctx, sessionOutboundEpochContextKey{}, epoch)
		}
		headers := http.Header{"Session-Id": {session}, "Thread-Id": {session}, codexTurnMetadataHeader: {fmt.Sprintf(`{"turn_id":%q}`, turn)}}
		ctx, err = withBPSFullConvergence(ctx, account, bpsProfile(profile), headers, nil, cache, "api-key")
		require.NoError(t, err)
		wire, d, err := prepareCodexBPSBodyForProfile(input, cache, false, false, headers, bpsProfile(profile), ctx)
		require.NoError(t, err)
		require.NotNil(t, d.FullConvergence)
		require.Equal(t, "upstream_account", d.FullConvergence.TaskScope)
		require.True(t, d.FullConvergence.Persisted)
		require.Equal(t, d.FullConvergence.TaskID, gjson.GetBytes(wire, "metadata.task_id").String())
		require.NotContains(t, string(wire), "full_convergence")
		return d.FullConvergence
	}
	first := project("user-a", "chat-a", "turn-a", "cache-a", auth.BPSWord, nil, body)
	for _, id := range []string{first.TaskID, first.TurnID} {
		parsed, err := uuid.Parse(id)
		require.NoError(t, err)
		require.EqualValues(t, 7, parsed.Version())
	}
	retry := project("user-a", "chat-a", "turn-a", "changed-cache", auth.BPSWord, nil, body)
	require.Equal(t, first.TaskID, retry.TaskID)
	require.Equal(t, first.TurnID, retry.TurnID)
	require.Equal(t, "1", retry.AgentIteration)
	require.True(t, retry.ReusedStep)
	for _, scenario := range []struct{ owner, session, turn string }{
		{"user-b", "chat-a", "turn-a"}, {"user-a", "chat-b", "turn-a"}, {"user-a", "chat-a", "turn-b"},
	} {
		next := project(scenario.owner, scenario.session, scenario.turn, "cache-a", auth.BPSWord, nil, body)
		require.Equal(t, first.TaskID, next.TaskID)
		require.NotEqual(t, first.TurnID, next.TurnID)
		require.Equal(t, "1", next.AgentIteration)
	}
	for _, profile := range []auth.CodexBPSProfile{auth.BPSExcel, auth.BPSSheets, auth.BPSPowerPoint} {
		next := project("user-a", "chat-a", "turn-a", "cache-a", profile, nil, body)
		require.Equal(t, first.TaskID, next.TaskID, "full convergence is one task per upstream account, including different BPS profiles")
		require.NotEqual(t, first.TurnID, next.TurnID)
	}
	tools := []byte(`{"model":"gpt-6-astra","input":[{"role":"user","content":"hello"},{"type":"function_call_output","call_id":"call-a","output":"done"}]}`)
	continued := project("user-a", "chat-a", "turn-a", "cache-a", auth.BPSWord, nil, tools)
	require.Equal(t, first.TurnID, continued.TurnID)
	require.Equal(t, "2", continued.AgentIteration)
	rebound := project("user-a", "chat-a", "turn-a", "cache-a", auth.BPSWord,
		&sessionOutboundEpoch{key: "chat-a", record: database.SessionContinuityRecord{OutboundWindowReset: true, FailoverCount: 2}}, tools)
	require.Equal(t, first.TaskID, rebound.TaskID, "returning to an account must reuse its shared task")
	require.NotEqual(t, first.TurnID, rebound.TurnID)
	require.Equal(t, "1", rebound.AgentIteration)
	account.DBID = 18
	duplicate := project("user-a", "chat-a", "turn-a", "cache-a", auth.BPSWord, nil, body)
	require.Equal(t, first.TaskID, duplicate.TaskID)
	require.Equal(t, first.TurnID, duplicate.TurnID)
	account.AccountID = "another-upstream-account"
	switched := project("user-a", "chat-a", "turn-a", "cache-a", auth.BPSWord, nil, body)
	require.NotEqual(t, first.TaskID, switched.TaskID)
	require.NotEqual(t, first.TurnID, switched.TurnID)
	account.AccountID = accountIdentitySampleAccount
	require.NoError(t, db.Close())
	db, err = database.New("sqlite", path)
	require.NoError(t, err)
	resumed := project("user-a", "chat-a", "turn-a", "cache-a", auth.BPSWord, nil, tools)
	require.Equal(t, first.TaskID, resumed.TaskID)
	require.Equal(t, continued.TurnID, resumed.TurnID)
	require.Equal(t, "2", resumed.AgentIteration)
}

func TestBPSFullConvergenceConcurrentTaskCreation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.db")
	first, err := database.New("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	second, err := database.New("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	account := &auth.Account{AccountID: accountIdentitySampleAccount, CodexFingerprintMode: auth.CodexFingerprintModeFull}
	type result struct {
		identity *bpsWordIdentityDiagnostic
		err      error
	}
	results := make(chan result, 8)
	for i := range 8 {
		go func() {
			db := []*database.DB{first, second}[i%2]
			ctx := context.WithValue(WithCodexIdentityStore(t.Context(), db), transportUserContextKey{}, fmt.Sprint("user-", i))
			ctx, err := withBPSFullConvergence(ctx, account, bpsProfile(auth.BPSWord), http.Header{}, nil, fmt.Sprint("cache-", i), "key")
			if err != nil {
				results <- result{err: err}
				return
			}
			d, err := resolveBPSWordIdentity(ctx, []byte(`{"input":"hello"}`), http.Header{}, "cache")
			results <- result{d, err}
		}()
	}
	var task string
	turns := make(map[string]bool)
	for range 8 {
		r := <-results
		require.NoError(t, r.err)
		if task == "" {
			task = r.identity.TaskID
		}
		require.Equal(t, task, r.identity.TaskID)
		require.False(t, turns[r.identity.TurnID])
		turns[r.identity.TurnID] = true
	}
}

func TestBPSFullConvergenceExecutorAndDisabledModes(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	for _, mode := range []string{"account", "preserve", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("CODEX_OUTBOUND_SESSION_MODE", mode)
			db, err := database.New("sqlite", filepath.Join(t.TempDir(), "executor.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			a := &auth.Account{DBID: 1695, AccountID: accountIdentitySampleAccount, AccessToken: "test-access", CodexBPS: true, CodexFingerprintMode: auth.CodexFingerprintModeFull, CodexInstallationID: "account-device"}
			var wire []byte
			installClaudeBoundaryTransport(t, a, func(r *http.Request) (*http.Response, error) {
				wire, err = io.ReadAll(r.Body)
				require.NoError(t, err)
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"output":[]}`)), Request: r}, nil
			})
			var task string
			for _, profile := range []auth.CodexBPSProfile{auth.BPSWord, auth.BPSExcel, auth.BPSSheets, auth.BPSPowerPoint} {
				a.CodexBPSProfile = profile
				var turn string
				for _, compact := range []bool{false, true} {
					headers, body := accountIdentityFixture(t, false, true)
					c := transportTestContext()
					ctx := WithCodexIdentityStore(c.Request.Context(), db)
					var resp *http.Response
					if compact {
						resp, err = ExecuteCompactRequest(ctx, a, body, "cache", "", "test-key", nil, headers)
					} else {
						resp, err = ExecuteRequest(ctx, a, body, "cache", "", "test-key", nil, headers, true)
					}
					require.NoError(t, err)
					require.NoError(t, resp.Body.Close())
					if task == "" {
						task = gjson.GetBytes(wire, "metadata.task_id").String()
					}
					require.Equal(t, task, gjson.GetBytes(wire, "metadata.task_id").String())
					if turn == "" {
						turn = gjson.GetBytes(wire, "metadata.turn_id").String()
					}
					require.Equal(t, turn, gjson.GetBytes(wire, "metadata.turn_id").String())
					require.NotNil(t, snapshotUpstreamTrace(ctx).Transport.BPS.FullConvergence)
					require.Contains(t, string(wire), "private-encrypted", "full convergence must not merge or remove caller history")
				}
			}
		})
	}
	for _, mode := range []string{auth.CodexFingerprintModeOff, auth.CodexFingerprintModeDevice, auth.CodexFingerprintModeSession} {
		stale := context.WithValue(t.Context(), bpsFullConvergenceKey{}, &bpsFullConvergenceScope{taskKey: "previous-account"})
		ctx, err := withBPSFullConvergence(stale, &auth.Account{CodexFingerprintMode: mode}, bpsProfile(auth.BPSWord), nil, nil, "cache", "key")
		require.NoError(t, err)
		require.Nil(t, bpsFullConvergenceFrom(ctx))
	}
	_, err := withBPSFullConvergence(t.Context(), &auth.Account{CodexFingerprintMode: auth.CodexFingerprintModeFull}, bpsProfile(auth.BPSWord), nil, nil, "cache", "key")
	require.ErrorContains(t, err, "上游账号 ID")
}
