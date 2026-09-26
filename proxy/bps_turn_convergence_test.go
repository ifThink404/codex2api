package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestBPSTurnConvergenceUserTurnsAndPartitions(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	settings := DefaultRuntimeSettings()
	settings.BPSRoundConvergenceLimit = 1 // The old mode's limit must not affect this mode.
	settings.BPSTurnTaskLifetimeHours = 36
	ApplyRuntimeSettings(settings)
	path := filepath.Join(t.TempDir(), "turns.db")
	db, err := database.New("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	a := &auth.Account{AccountID: accountIdentitySampleAccount, CodexFingerprintMode: auth.CodexFingerprintModeTurnRound}
	project := func(owner, session, turn, model, effort, input string, compact bool) *bpsWordIdentityDiagnostic {
		t.Helper()
		headers := http.Header{"Session-Id": {session}}
		if turn != "" {
			headers.Set(codexTurnMetadataHeader, fmt.Sprintf(`{"turn_id":%q}`, turn))
		}
		ctx := context.WithValue(WithCodexIdentityStore(t.Context(), db), transportUserContextKey{}, owner)
		ctx, err := withBPSFullConvergence(ctx, a, bpsProfile(auth.BPSWord), headers, nil, "cache", "key")
		require.NoError(t, err)
		body := []byte(fmt.Sprintf(`{"model":%q,"reasoning":{"effort":%q},"input":%s}`, model, effort, input))
		wire, diag, err := prepareCodexBPSBodyForProfile(body, "cache", compact, false, headers, bpsProfile(auth.BPSWord), ctx)
		require.NoError(t, err)
		require.Nil(t, diag.FullConvergence)
		require.Nil(t, diag.RoundConvergence)
		d := diag.TurnConvergence
		require.NotNil(t, d)
		require.Equal(t, diag.WordIdentity, d)
		require.Equal(t, "upstream_account_model_effort_timed_turns", d.TaskScope)
		require.Equal(t, 36, d.TaskLifetimeHours)
		require.Zero(t, d.RoundLimit)
		require.Equal(t, d.TaskID, gjson.GetBytes(wire, "metadata.task_id").String())
		require.Equal(t, d.TurnID, gjson.GetBytes(wire, "metadata.turn_id").String())
		require.Equal(t, d.AgentIteration, gjson.GetBytes(wire, "metadata.agent_iteration").String())
		return d
	}
	const initial = `[{"role":"user","content":"hello"}]`
	const tool = `[{"role":"user","content":"hello"},{"type":"function_call_output","call_id":"a","output":"done"}]`
	first := project("alice", "session-a", "turn-a", "gpt-6-astra", "low", initial, false)
	second := project("alice", "session-a", "turn-a", "gpt-6-astra", "low", tool, false)
	require.Equal(t, first.TaskID, second.TaskID)
	require.Equal(t, first.TurnID, second.TurnID)
	require.Equal(t, "1", first.AgentIteration)
	require.Equal(t, "2", second.AgentIteration)
	for _, id := range []string{first.TaskID, first.TurnID} {
		parsed, err := uuid.Parse(id)
		require.NoError(t, err)
		require.EqualValues(t, 7, parsed.Version())
	}
	// Compact is a distinct step, but not a new user turn.
	compact := project("alice", "session-a", "turn-a", "gpt-6-astra", "low", tool, true)
	require.Equal(t, first.TurnID, compact.TurnID)
	require.Equal(t, "3", compact.AgentIteration)
	next := project("alice", "session-a", "turn-b", "gpt-6-astra", "low", initial, false)
	require.Equal(t, first.TaskID, next.TaskID)
	require.NotEqual(t, first.TurnID, next.TurnID)
	require.Equal(t, "1", next.AgentIteration)
	for _, user := range [][2]string{{"bob", "session-a"}, {"alice", "session-b"}} {
		d := project(user[0], user[1], "turn-a", "gpt-6-astra", "low", initial, false)
		require.Equal(t, first.TaskID, d.TaskID)
		require.NotEqual(t, first.TurnID, d.TurnID, "shared task must not collapse different users or conversations into one turn")
		require.Equal(t, "1", d.AgentIteration)
	}
	tasks := map[string]bool{}
	for _, model := range []string{"gpt-6-astra", "gpt-5.6-sol"} {
		for _, effort := range []string{"low", "medium", "high", "xhigh"} {
			d := project("alice", "session-a", "turn-a", model, effort, initial, false)
			require.False(t, tasks[d.TaskID])
			tasks[d.TaskID] = true
			require.Equal(t, effort, d.TaskReasoningEffort)
			require.Equal(t, "1", d.AgentIteration)
		}
	}
	require.Len(t, tasks, 8)
	for _, effort := range []string{"", "max"} {
		d := project("alice", "session-a", "turn-a", "gpt-6-astra", effort, initial, false)
		require.True(t, tasks[d.TaskID])
		require.True(t, d.ReusedStep)
	}
	a.AccountID = "other-account"
	other := project("alice", "session-a", "turn-a", "gpt-6-astra", "low", initial, false)
	require.NotEqual(t, first.TaskID, other.TaskID)
	require.NotEqual(t, first.TurnID, other.TurnID)
	a.AccountID = accountIdentitySampleAccount
	require.NoError(t, db.Close())
	db, err = database.New("sqlite", path)
	require.NoError(t, err)
	retry := project("alice", "session-a", "turn-a", "gpt-6-astra", "low", tool, false)
	require.Equal(t, second.TaskID, retry.TaskID)
	require.Equal(t, second.TurnID, retry.TurnID)
	require.Equal(t, "2", retry.AgentIteration)
	require.True(t, retry.ReusedStep)
	// Clients without turn IDs fall back to the last user-message boundary.
	fallback := project("alice", "fallback", "", "gpt-6-astra", "low", initial, false)
	fallbackTool := project("alice", "fallback", "", "gpt-6-astra", "low", tool, false)
	fallbackNext := project("alice", "fallback", "", "gpt-6-astra", "low", `[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"hello again"}]`, false)
	require.Equal(t, "user_boundary", fallback.TurnSource)
	require.Equal(t, fallback.TurnID, fallbackTool.TurnID)
	require.Equal(t, "2", fallbackTool.AgentIteration)
	require.NotEqual(t, fallback.TurnID, fallbackNext.TurnID)
	require.Equal(t, "1", fallbackNext.AgentIteration)
}

// Simulate a task generation change at the durable assignment boundary; the
// database suite separately exercises real deadline and concurrent rotation.
type turnGenerationTestStore struct {
	*database.DB
	generation  int64
	assignments map[string]database.BPSTurnTaskIdentity
}

func (s *turnGenerationTestStore) ResolveBPSTurnTaskIdentity(_ context.Context, partition, step string, hours int) (database.BPSTurnTaskIdentity, bool, error) {
	key := partition + step
	if old, ok := s.assignments[key]; ok {
		return old, true, nil
	}
	value := database.BPSTurnTaskIdentity{Generation: s.generation, LifetimeHours: hours}
	s.assignments[key] = value
	return value, false, nil
}

func TestBPSTurnConvergenceRotationAndReplay(t *testing.T) {
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "rotation.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	store := &turnGenerationTestStore{DB: db, assignments: map[string]database.BPSTurnTaskIdentity{}}
	ctx := WithCodexIdentityStore(t.Context(), store)
	scope := &bpsFullConvergenceScope{taskKey: "account", taskLifetimeHours: 24}
	resolve := func(step string) *bpsWordIdentityDiagnostic {
		d, err := resolveBPSTurnIdentity(ctx, scope, "gpt-6-astra", "low", "same-user-turn", codexIdentityDigest(step), &bpsWordIdentityDiagnostic{})
		require.NoError(t, err)
		return d
	}
	first := resolve("step1")
	store.generation = 1
	second := resolve("step2")
	require.NotEqual(t, first.TaskID, second.TaskID)
	require.NotEqual(t, first.TurnID, second.TurnID)
	require.Equal(t, "1", second.AgentIteration)
	retry := resolve("step1")
	require.Equal(t, first.TaskID, retry.TaskID)
	require.Equal(t, first.TurnID, retry.TurnID)
	require.Equal(t, "1", retry.AgentIteration)
	require.True(t, retry.ReusedStep)
}

func TestBPSTurnConvergenceExecutor(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	settings := DefaultRuntimeSettings()
	settings.BPSTurnTaskLifetimeHours = 2
	ApplyRuntimeSettings(settings)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "executor.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	a := &auth.Account{DBID: 1769, AccountID: accountIdentitySampleAccount, AccessToken: "test-access", CodexBPS: true, CodexFingerprintMode: auth.CodexFingerprintModeTurnRound}
	var wire []byte
	installClaudeBoundaryTransport(t, a, func(r *http.Request) (*http.Response, error) {
		require.Empty(t, r.Header.Get("Session-Id"))
		require.Empty(t, r.Header.Get(codexTurnMetadataHeader))
		wire, err = io.ReadAll(r.Body)
		require.NoError(t, err)
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"output":[]}`)), Request: r}, nil
	})
	var first *bpsWordIdentityDiagnostic
	for i := range 2 {
		headers, body := accountIdentityFixture(t, false, true)
		if i == 1 {
			oldTurn := gjson.Get(headers.Get(codexTurnMetadataHeader), "turn_id").String()
			newTurn := "01a0939f-d89c-77f1-94fa-000000000123"
			headers.Set(codexTurnMetadataHeader, strings.ReplaceAll(headers.Get(codexTurnMetadataHeader), oldTurn, newTurn))
			body = []byte(strings.ReplaceAll(string(body), oldTurn, newTurn))
		}
		c := transportTestContext()
		ctx := WithCodexIdentityStore(c.Request.Context(), db)
		resp, err := ExecuteRequest(ctx, a, body, "cache", "", "test-key", nil, headers, true)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		d := CodexBPSResponseDiagnostic(resp).TurnConvergence
		require.NotNil(t, d)
		require.Positive(t, d.TaskStartedAtMS)
		require.Equal(t, (2 * time.Hour).Milliseconds(), d.TaskExpiresAtMS-d.TaskStartedAtMS)
		require.Equal(t, "1", gjson.GetBytes(wire, "metadata.agent_iteration").String())
		require.Equal(t, d.TurnID, gjson.GetBytes(wire, "metadata.turn_id").String())
		require.False(t, gjson.GetBytes(wire, "prompt_cache_key").Exists())
		require.False(t, gjson.GetBytes(wire, "client_metadata").Exists())
		require.NotNil(t, snapshotUpstreamTrace(ctx).Transport.BPS.TurnConvergence)
		if first != nil {
			require.Equal(t, first.TaskID, d.TaskID)
			require.NotEqual(t, first.TurnID, d.TurnID)
			require.Equal(t, first.TaskExpiresAtMS, d.TaskExpiresAtMS)
		}
		first = d
	}
}

func TestBPSTurnConvergenceNeedsStoreAndLeavesNativeSeparate(t *testing.T) {
	a := fingerprintAccount(t, auth.CodexFingerprintModeTurnRound)
	a.AccountID = accountIdentitySampleAccount
	ids := resolveCodexFingerprintIDs(a, codexClientHeaders("", "client-session"))
	require.NotEmpty(t, ids.installationID)
	require.Empty(t, ids.sessionID)
	require.Empty(t, ids.threadID)
	ctx, err := withBPSFullConvergence(t.Context(), a, bpsProfile(auth.BPSWord), nil, nil, "cache", "key")
	require.NoError(t, err)
	_, err = resolveBPSWordIdentity(ctx, []byte(`{"input":"hello"}`), nil, "cache", "gpt-6-astra", false)
	require.ErrorContains(t, err, "持久化")
}
