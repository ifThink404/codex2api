package proxy

import (
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestBPSRoundConvergenceIdentityLifecycle(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	settings := DefaultRuntimeSettings()
	bpsSettings := BPSConfig{}
	previousBPS := currentBPSConfig()
	t.Cleanup(func() { storeBPSConfig(previousBPS) })
	bpsSettings.RoundConvergenceLimit = 3
	ApplyRuntimeSettings(settings)
	storeBPSConfig(bpsSettings)
	path := filepath.Join(t.TempDir(), "rounds.db")
	db, err := newBPSProxyTestDB("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	a := (&auth.Account{DBID: 24, AccountID: accountIdentitySampleAccount}).SetCodexBPSOptions(auth.CodexBPSAccountOptions{Convergence: auth.CodexBPSConvergenceRound})
	project := func(owner, session, turn string, profile auth.CodexBPSProfile, body string) *bpsWordIdentityDiagnostic {
		t.Helper()
		ctx := withBPSTestUser(WithCodexIdentityStore(t.Context(), db), owner)
		headers := http.Header{"Session-Id": {session}, codexTurnMetadataHeader: {fmt.Sprintf(`{"turn_id":%q}`, turn)}}
		ctx, err := withBPSFullConvergenceTest(ctx, a, bpsProfile(profile), headers, nil, "cache", "key")
		require.NoError(t, err)
		wire, diag, err := prepareCodexBPSBodyForProfile([]byte(body), "cache", false, false, headers, bpsProfile(profile), ctx)
		require.NoError(t, err)
		require.Nil(t, diag.FullConvergence)
		require.NotNil(t, diag.RoundConvergence)
		d := diag.RoundConvergence
		require.Equal(t, "upstream_account_model_effort_rounds", d.TaskScope)
		require.Equal(t, "gpt-6-astra", d.TaskModel)
		require.Equal(t, "low", d.TaskReasoningEffort)
		require.Equal(t, d.AgentIteration, gjson.GetBytes(wire, "metadata.agent_iteration").String())
		require.Equal(t, d.TaskID, gjson.GetBytes(wire, "metadata.task_id").String())
		require.Equal(t, d.TurnID, gjson.GetBytes(wire, "metadata.turn_id").String())
		for _, id := range []string{d.TaskID, d.TurnID} {
			parsed, err := uuid.Parse(id)
			require.NoError(t, err)
			require.EqualValues(t, 7, parsed.Version())
		}
		return d
	}
	const initial = `{"model":"gpt-6-astra","input":[{"role":"user","content":"hello"}]}`
	const tool = `{"model":"gpt-6-astra","input":[{"role":"user","content":"hello"},{"type":"function_call_output","call_id":"call-a","output":"done"}]}`
	first := project("user-a", "chat-a", "turn-a", auth.BPSWord, initial)
	second := project("user-a", "chat-a", "turn-a", auth.BPSWord, tool)
	require.Equal(t, first.TaskID, second.TaskID)
	require.Equal(t, first.TurnID, second.TurnID, "all calls in one task batch share a fixed turn")
	require.Equal(t, "2", second.AgentIteration)
	third := project("user-b", "chat-b", "turn-b", auth.BPSExcel, initial)
	require.Equal(t, first.TaskID, third.TaskID, "users and profiles share the account's batch counter")
	require.Equal(t, first.TurnID, third.TurnID)
	require.Equal(t, "3", third.AgentIteration)
	fourth := project("user-a", "chat-a", "turn-next", auth.BPSWord, initial)
	require.NotEqual(t, first.TaskID, fourth.TaskID)
	require.NotEqual(t, first.TurnID, fourth.TurnID)
	require.Equal(t, "1", fourth.AgentIteration)
	require.EqualValues(t, 1, fourth.TaskGeneration)
	retry := project("user-a", "chat-a", "turn-a", auth.BPSWord, tool)
	require.Equal(t, second.TaskID, retry.TaskID)
	require.Equal(t, second.TurnID, retry.TurnID)
	require.Equal(t, "2", retry.AgentIteration)
	require.True(t, retry.ReusedStep)
	require.NoError(t, db.Close())
	db, err = newBPSProxyTestDB("sqlite", path)
	require.NoError(t, err)
	next := project("user-b", "chat-b", "turn-next", auth.BPSSheets, initial)
	require.Equal(t, fourth.TaskID, next.TaskID)
	require.Equal(t, fourth.TurnID, next.TurnID)
	require.Equal(t, "2", next.AgentIteration)
	a.AccountID = "another-upstream-account"
	other := project("user-a", "chat-a", "turn-a", auth.BPSWord, initial)
	require.NotEqual(t, first.TaskID, other.TaskID)
	require.Equal(t, "1", other.AgentIteration)
	a.AccountID = accountIdentitySampleAccount
	a.DBID = 25
	resumed := project("user-a", "chat-a", "turn-final", auth.BPSWord, initial)
	require.Equal(t, fourth.TaskID, resumed.TaskID, "duplicate local account rows share real upstream identity")
	require.Equal(t, "3", resumed.AgentIteration)
}

func TestBPSRoundConvergenceNeedsDurableStore(t *testing.T) {
	a := (&auth.Account{AccountID: accountIdentitySampleAccount}).SetCodexBPSOptions(auth.CodexBPSAccountOptions{Convergence: auth.CodexBPSConvergenceRound})
	ctx, err := withBPSFullConvergenceTest(t.Context(), a, bpsProfile(auth.BPSWord), nil, nil, "cache", "key")
	require.NoError(t, err)
	_, err = resolveBPSWordIdentity(ctx, []byte(`{"input":"hello"}`), nil, "cache", "gpt-6-astra", false)
	require.ErrorContains(t, err, "持久化")
}

// BPS convergence is its own account key; it never changes the native
// fingerprint mode, so native sessions are untouched.
func TestRoundConvergenceLeavesNativeSessionsSeparate(t *testing.T) {
	a := (&auth.Account{DBID: 1}).SetCodexBPSOptions(auth.CodexBPSAccountOptions{Convergence: auth.CodexBPSConvergenceRound})
	require.Nil(t, resolveCodexFingerprintIDs(a, codexClientHeaders("", "client-session")))
}

func TestBPSRoundConvergenceCompactionHasOwnStep(t *testing.T) {
	db, err := newBPSProxyTestDB("sqlite", filepath.Join(t.TempDir(), "compact.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	a := (&auth.Account{AccountID: accountIdentitySampleAccount}).SetCodexBPSOptions(auth.CodexBPSAccountOptions{Convergence: auth.CodexBPSConvergenceRound})
	ctx, err := withBPSFullConvergenceTest(WithCodexIdentityStore(t.Context(), db), a, bpsProfile(auth.BPSWord), nil, nil, "cache", "key")
	require.NoError(t, err)
	body := []byte(`{"input":"hello"}`)
	first, err := resolveBPSWordIdentity(ctx, body, nil, "cache", "gpt-6-astra", false)
	require.NoError(t, err)
	compact, err := resolveBPSWordIdentity(ctx, body, nil, "cache", "gpt-6-astra", true)
	require.NoError(t, err)
	require.Equal(t, first.TaskID, compact.TaskID)
	require.Equal(t, first.TurnID, compact.TurnID)
	require.Equal(t, "2", compact.AgentIteration)
	retry, err := resolveBPSWordIdentity(ctx, body, nil, "cache", "gpt-6-astra", true)
	require.NoError(t, err)
	require.Equal(t, compact.TurnID, retry.TurnID)
	require.True(t, retry.ReusedStep)
}

func TestBPSRoundConvergenceExecutor(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	settings := DefaultRuntimeSettings()
	bpsSettings := BPSConfig{}
	previousBPS := currentBPSConfig()
	t.Cleanup(func() { storeBPSConfig(previousBPS) })
	bpsSettings.RoundConvergenceLimit = 2
	bpsSettings.RoundTaskLifetimeHours = 7
	ApplyRuntimeSettings(settings)
	storeBPSConfig(bpsSettings)
	db, err := newBPSProxyTestDB("sqlite", filepath.Join(t.TempDir(), "executor.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	a := withBPSOverride((&auth.Account{DBID: 1699, AccountID: accountIdentitySampleAccount, AccessToken: "test-access"}).SetCodexBPSOptions(auth.CodexBPSAccountOptions{Convergence: auth.CodexBPSConvergenceRound}), true)
	var wire []byte
	var outboundHeaders http.Header
	installClaudeBoundaryTransport(t, a, func(r *http.Request) (*http.Response, error) {
		outboundHeaders = r.Header.Clone()
		wire, err = io.ReadAll(r.Body)
		require.NoError(t, err)
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"output":[]}`)), Request: r}, nil
	})
	var firstTask string
	var firstStartedAtMS int64
	for i := range 3 {
		headers, body := accountIdentityFixture(t, false, true)
		oldTurn := gjson.Get(headers.Get(codexTurnMetadataHeader), "turn_id").String()
		require.NotEmpty(t, oldTurn)
		newTurn := fmt.Sprintf("01a0939f-d89c-77f1-94fa-%012d", i+1)
		headers.Set(codexTurnMetadataHeader, strings.ReplaceAll(headers.Get(codexTurnMetadataHeader), oldTurn, newTurn))
		body = []byte(strings.ReplaceAll(string(body), oldTurn, newTurn))
		c := transportTestContext()
		ctx := WithCodexIdentityStore(c.Request.Context(), db)
		ctx = withBPSTestUser(ctx, "round-user")
		resp, err := executeBPSTestRequest(ctx, a, body, "cache", "", "test-key", nil, headers, true)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Empty(t, outboundHeaders.Get("Session-Id"), "Word BPS uses task_id, not a native session header")
		require.Empty(t, outboundHeaders.Get("Session_id"))
		require.Empty(t, outboundHeaders.Get(codexTurnMetadataHeader))
		require.False(t, gjson.GetBytes(wire, "prompt_cache_key").Exists())
		require.False(t, gjson.GetBytes(wire, "client_metadata").Exists())
		task := gjson.GetBytes(wire, "metadata.task_id").String()
		d := CodexBPSResponseDiagnostic(resp).RoundConvergence
		require.Positive(t, d.TaskStartedAtMS)
		require.GreaterOrEqual(t, d.TaskLastSentAtMS, d.TaskStartedAtMS)
		require.Equal(t, int64(7*60*60*1000), d.TaskExpiresAtMS-d.TaskStartedAtMS)
		if i == 0 {
			firstTask = task
			firstStartedAtMS = d.TaskStartedAtMS
		}
		if i < 2 {
			require.Equal(t, firstTask, task)
			require.Equal(t, firstStartedAtMS, d.TaskStartedAtMS, "activity preserves the initial send time")
		} else {
			require.NotEqual(t, firstTask, task)
		}
		require.Equal(t, fmt.Sprint(i%2+1), gjson.GetBytes(wire, "metadata.agent_iteration").String())
		require.NotNil(t, bpsTraceTransport(ctx).BPS.RoundConvergence)
		require.Contains(t, string(wire), "private-encrypted")
	}
}
