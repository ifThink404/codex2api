package proxy

import (
	"fmt"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestBPSRoundConvergenceModelEffortPartitions(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	settings := DefaultRuntimeSettings()
	settings.BPSRoundConvergenceLimit = 2
	ApplyRuntimeSettings(settings)
	path := filepath.Join(t.TempDir(), "effort-rounds.db")
	db, err := database.New("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	account := &auth.Account{AccountID: accountIdentitySampleAccount, CodexFingerprintMode: auth.CodexFingerprintModeRound}
	project := func(model, effort, turn string) *bpsWordIdentityDiagnostic {
		t.Helper()
		headers := http.Header{"Session-Id": {"source-session"}, codexTurnMetadataHeader: {fmt.Sprintf(`{"turn_id":%q}`, turn)}}
		ctx, err := withBPSFullConvergence(WithCodexIdentityStore(t.Context(), db), account, bpsProfile(auth.BPSWord), headers, nil, "cache", "key")
		require.NoError(t, err)
		body := []byte(fmt.Sprintf(`{"model":%q,"reasoning":{"effort":%q},"input":"same input"}`, model, effort))
		wire, diag, err := prepareCodexBPSBodyForProfile(body, "cache", false, false, headers, bpsProfile(auth.BPSWord), ctx)
		require.NoError(t, err)
		require.NotNil(t, diag.RoundConvergence)
		require.Equal(t, gjson.GetBytes(wire, "reasoning_effort").String(), diag.RoundConvergence.TaskReasoningEffort)
		require.Equal(t, diag.RoundConvergence.TaskID, gjson.GetBytes(wire, "metadata.task_id").String())
		require.False(t, gjson.GetBytes(wire, "prompt_cache_key").Exists())
		require.False(t, gjson.GetBytes(wire, "client_metadata").Exists())
		return diag.RoundConvergence
	}
	first := map[string]*bpsWordIdentityDiagnostic{}
	tasks := map[string]bool{}
	for _, model := range []string{"gpt-6-astra", "gpt-5.6-sol"} {
		for _, effort := range []string{"low", "medium", "high", "xhigh"} {
			d := project(model, effort, "turn-1")
			require.False(t, tasks[d.TaskID], "different model/effort combinations must have separate tasks: %s/%s", model, effort)
			tasks[d.TaskID] = true
			first[model+"/"+effort] = d
			require.Equal(t, "1", d.AgentIteration)
		}
	}
	require.Len(t, tasks, 8)
	for _, alias := range []struct{ requested, effective string }{{"max", "xhigh"}, {"", "low"}} {
		d := project("gpt-6-astra", alias.requested, "turn-1")
		original := first["gpt-6-astra/"+alias.effective]
		require.Equal(t, original.TaskID, d.TaskID)
		require.Equal(t, original.TurnID, d.TurnID)
		require.True(t, d.ReusedStep)
	}
	require.NoError(t, db.Close())
	db, err = database.New("sqlite", path)
	require.NoError(t, err)
	for _, model := range []string{"gpt-6-astra", "gpt-5.6-sol"} {
		for _, effort := range []string{"low", "medium", "high", "xhigh"} {
			d := project(model, effort, "turn-2")
			require.Equal(t, first[model+"/"+effort].TaskID, d.TaskID)
			require.Equal(t, "2", d.AgentIteration)
		}
	}
	rotated := project("gpt-6-astra", "low", "turn-3")
	require.NotEqual(t, first["gpt-6-astra/low"].TaskID, rotated.TaskID)
	require.Equal(t, "1", rotated.AgentIteration)
	retry := project("gpt-6-astra", "low", "turn-1")
	require.Equal(t, first["gpt-6-astra/low"].TaskID, retry.TaskID)
	require.True(t, retry.ReusedStep)
}

func TestBPSRoundConvergenceUsesConfigurationEffort(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	ApplyRuntimeSettings(DefaultRuntimeSettings())
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "configuration.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	account := &auth.Account{AccountID: accountIdentitySampleAccount, CodexFingerprintMode: auth.CodexFingerprintModeRound}
	headers := http.Header{codexTurnMetadataHeader: {`{"turn_id":"same-turn"}`}}
	ctx, err := withBPSFullConvergence(WithCodexIdentityStore(t.Context(), db), account, bpsProfile(auth.BPSWord), headers, nil, "cache", "key")
	require.NoError(t, err)
	var xhighTask string
	for i, body := range []string{
		`{"model":"gpt-6-astra","reasoning":{"effort":"xhigh"},"input":"hello"}`,
		`{"model":"gpt-6-astra","reasoning":{"effort":"low"},"input":[{"type":"configuration_update","reasoning":{"effort":"high"}},{"type":"configuration_update","reasoning":{"effort":"max"}},{"role":"user","content":"hello"}]}`,
	} {
		_, d, err := prepareCodexBPSBodyForProfile([]byte(body), "cache", false, false, headers, bpsProfile(auth.BPSWord), ctx)
		require.NoError(t, err)
		if i == 0 {
			xhighTask = d.RoundConvergence.TaskID
		}
		require.Equal(t, "xhigh", d.RoundConvergence.TaskReasoningEffort)
		require.Equal(t, xhighTask, d.RoundConvergence.TaskID, "last effective configuration update selects the effort partition")
	}
	_, low, err := prepareCodexBPSBodyForProfile([]byte(`{"model":"gpt-6-astra","reasoning_effort":"low","input":[{"role":"user","content":"hello"},{"type":"function_call_output","call_id":"a","output":{"type":"configuration_update","reasoning":{"effort":"max"}}}]}`), "cache", false, false, headers, bpsProfile(auth.BPSWord), ctx)
	require.NoError(t, err)
	require.Equal(t, "low", low.RoundConvergence.TaskReasoningEffort)
	require.NotEqual(t, xhighTask, low.RoundConvergence.TaskID, "tool data must not change the request's effort partition")
}
