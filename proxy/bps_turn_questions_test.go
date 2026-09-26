package proxy

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
)

func TestBPSTurnQuestionsRotateWithoutRotatingTask(t *testing.T) {
	path := filepath.Join(t.TempDir(), "turn-questions.db")
	db, err := database.New("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	scope := &bpsFullConvergenceScope{taskKey: "account", userScope: "alice", taskLifetimeHours: 24, turnRoundLimit: 2}
	resolve := func(question, operation string) *bpsWordIdentityDiagnostic {
		t.Helper()
		ctx := WithCodexIdentityStore(context.Background(), db)
		d, err := resolveBPSTurnIdentity(ctx, scope, "gpt-6-astra", "low", codexIdentityDigest("window", question, scope.turnEpoch), codexIdentityDigest(operation), codexIdentityDigest("window", question), &bpsWordIdentityDiagnostic{})
		require.NoError(t, err)
		return d
	}
	first := resolve("q1", "initial")
	require.Equal(t, "1", first.AgentIteration)
	for i := 0; i < 5; i++ {
		tool := resolve("q1", fmt.Sprintf("tool-%d", i))
		require.Equal(t, first.TurnID, tool.TurnID)
		require.EqualValues(t, 1, tool.TurnQuestionNumber)
	}
	second := resolve("q2", "initial")
	require.Equal(t, first.TaskID, second.TaskID)
	require.Equal(t, first.TurnID, second.TurnID)
	require.EqualValues(t, 2, second.TurnQuestionNumber)
	require.Equal(t, "7", second.AgentIteration, "question count and inference iteration are distinct")
	scope.turnRoundLimit = 3
	third := resolve("q3", "initial")
	require.Equal(t, first.TaskID, third.TaskID, "question rotation must not rotate the timed task")
	require.NotEqual(t, first.TurnID, third.TurnID)
	require.Equal(t, "1", third.AgentIteration)
	require.EqualValues(t, 1, third.TurnQuestionNumber)
	require.EqualValues(t, 1, third.TurnGeneration)
	require.Equal(t, 3, third.TurnQuestionLimit)
	late := resolve("q1", "late-tool")
	require.Equal(t, first.TurnID, late.TurnID)
	require.EqualValues(t, 1, late.TurnQuestionNumber)
	require.Equal(t, "8", late.AgentIteration)
	require.NoError(t, db.Close())
	db, err = database.New("sqlite", path)
	require.NoError(t, err)
	retry := resolve("q2", "initial")
	require.Equal(t, second.TurnID, retry.TurnID)
	require.Equal(t, second.AgentIteration, retry.AgentIteration)
	require.True(t, retry.ReusedStep)
	// Returning after a switch keeps the question assignment but uses a fresh
	// outbound turn and iteration, without consuming the question twice.
	scope.turnEpoch = "returned-account"
	back := resolve("q2", "initial")
	require.Equal(t, second.TaskID, back.TaskID)
	require.NotEqual(t, second.TurnID, back.TurnID)
	require.EqualValues(t, 2, back.TurnQuestionNumber)
	require.Equal(t, "1", back.AgentIteration)
}
