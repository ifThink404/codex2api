package proxy

import (
	"fmt"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestBPSRoundConvergenceModelsRotateIndependently(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	settings := DefaultRuntimeSettings()
	bpsSettings := BPSConfig{}
	previousBPS := currentBPSConfig()
	t.Cleanup(func() { storeBPSConfig(previousBPS) })
	bpsSettings.RoundConvergenceLimit = 2
	ApplyRuntimeSettings(settings)
	storeBPSConfig(bpsSettings)
	path := filepath.Join(t.TempDir(), "model-rounds.db")
	db, err := newBPSProxyTestDB("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	account := (&auth.Account{AccountID: accountIdentitySampleAccount}).SetCodexBPSOptions(auth.CodexBPSAccountOptions{Convergence: auth.CodexBPSConvergenceRound})
	project := func(model, turn string) *bpsWordIdentityDiagnostic {
		t.Helper()
		headers := http.Header{"Session-Id": {"same-session"}, codexTurnMetadataHeader: {fmt.Sprintf(`{"turn_id":%q}`, turn)}}
		ctx, err := withBPSFullConvergenceTest(WithCodexIdentityStore(t.Context(), db), account, bpsProfile(auth.BPSWord), headers, nil, "same-cache", "same-key")
		require.NoError(t, err)
		wire, diag, err := prepareCodexBPSBodyForProfile([]byte(fmt.Sprintf(`{"model":%q,"input":"same input"}`, model)), "same-cache", false, false, headers, bpsProfile(auth.BPSWord), ctx)
		require.NoError(t, err)
		require.NotNil(t, diag.RoundConvergence)
		require.Equal(t, model, diag.RoundConvergence.TaskModel)
		require.Equal(t, diag.RoundConvergence.TaskID, gjson.GetBytes(wire, "metadata.task_id").String())
		require.Equal(t, diag.RoundConvergence.AgentIteration, gjson.GetBytes(wire, "metadata.agent_iteration").String())
		return diag.RoundConvergence
	}

	astra1 := project("gpt-6-astra", "turn-1")
	sol1 := project("gpt-5.6-sol", "turn-1")
	require.NotEqual(t, astra1.TaskID, sol1.TaskID, "same input and turn on different models must have separate task batches")
	require.NotEqual(t, astra1.TurnID, sol1.TurnID)
	require.Equal(t, "1", astra1.AgentIteration)
	require.Equal(t, "1", sol1.AgentIteration)
	require.False(t, sol1.ReusedStep, "switching model is a new call, not a retry of the other model")
	astra2 := project("gpt-6-astra", "turn-2")
	require.Equal(t, astra1.TaskID, astra2.TaskID)
	require.Equal(t, "2", astra2.AgentIteration)
	astra3 := project("gpt-6-astra", "turn-3")
	require.NotEqual(t, astra1.TaskID, astra3.TaskID)
	require.Equal(t, "1", astra3.AgentIteration)
	require.EqualValues(t, 1, astra3.TaskGeneration)

	// A process restart must resume both model counters independently.
	require.NoError(t, db.Close())
	db, err = newBPSProxyTestDB("sqlite", path)
	require.NoError(t, err)
	sol2 := project("gpt-5.6-sol", "turn-2")
	require.Equal(t, sol1.TaskID, sol2.TaskID, "Astra rotation must not rotate Sol")
	require.Equal(t, "2", sol2.AgentIteration)
	astra4 := project("gpt-6-astra", "turn-4")
	require.Equal(t, astra3.TaskID, astra4.TaskID)
	require.Equal(t, "2", astra4.AgentIteration)
	retry := project("gpt-6-astra", "turn-1")
	require.Equal(t, astra1.TaskID, retry.TaskID)
	require.Equal(t, astra1.TurnID, retry.TurnID)
	require.Equal(t, "1", retry.AgentIteration)
	require.True(t, retry.ReusedStep)
	sol3 := project("gpt-5.6-sol", "turn-3")
	require.NotEqual(t, sol1.TaskID, sol3.TaskID)
	require.NotEqual(t, astra3.TaskID, sol3.TaskID)
	require.Equal(t, "1", sol3.AgentIteration)
}

func TestBPSRoundConvergenceUsesSentModel(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	ApplyRuntimeSettings(DefaultRuntimeSettings())
	db, err := newBPSProxyTestDB("sqlite", filepath.Join(t.TempDir(), "sent-model.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	account := (&auth.Account{AccountID: accountIdentitySampleAccount}).SetCodexBPSOptions(auth.CodexBPSAccountOptions{Convergence: auth.CodexBPSConvergenceRound})
	ctx, err := withBPSFullConvergenceTest(WithCodexIdentityStore(t.Context(), db), account, bpsProfile(auth.BPSWord), nil, nil, "cache", "key")
	require.NoError(t, err)
	var task string
	for i, model := range []string{"codex-auto-review", "gpt-5.6-luna"} {
		body := []byte(fmt.Sprintf(`{"model":%q,"input":"call %d"}`, model, i))
		wire, diag, err := prepareCodexBPSBodyForProfile(body, "cache", false, false, nil, bpsProfile(auth.BPSWord), ctx)
		require.NoError(t, err)
		require.Equal(t, "gpt-5.6-luna", gjson.GetBytes(wire, "model").String())
		require.Equal(t, "gpt-5.6-luna", diag.RoundConvergence.TaskModel)
		if i == 0 {
			task = diag.RoundConvergence.TaskID
		}
		require.Equal(t, task, diag.RoundConvergence.TaskID, "aliases for the same sent model share its counter")
		require.Equal(t, fmt.Sprint(i+1), diag.RoundConvergence.AgentIteration)
	}
	_, other, err := prepareCodexBPSBodyForProfile([]byte(`{"model":"gpt-6-astra","input":"call 0"}`), "cache", false, false, nil, bpsProfile(auth.BPSWord), ctx)
	require.NoError(t, err)
	require.NotEqual(t, task, other.RoundConvergence.TaskID)
	require.Equal(t, "1", other.RoundConvergence.AgentIteration)
}
