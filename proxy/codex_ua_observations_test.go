package proxy

import (
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
)

const observedDesktopUA = "Codex Desktop/0.155.0-alpha.9 (Windows 10.0.26200; x86_64) unknown (Codex Desktop; 26.915.31029)"

func TestCodexUAObservationsUseActualGeneratedVersion(t *testing.T) {
	at := time.Now().UTC()
	index := NewCodexUserAgentObservations([]database.UsageClientUserAgentSample{{UserAgent: observedDesktopUA, Count: 4, LastSeen: at}}, at)
	config := `{"mode":"multi","client_kind":"codex-desktop","profiles":{"codex-desktop":{"client_version":"0.155.0-alpha.9","app_version":"26.915.31029"}}}`
	preview, err := PreviewCodexUserAgentConfig(config, "", nil)
	require.NoError(t, err)
	before := *preview.Persona
	index.Apply(&preview)
	observation := preview.Persona.Observation
	require.Equal(t, "matched", observation.Status)
	require.EqualValues(t, 4, observation.MatchCount)
	require.EqualValues(t, 4, observation.VersionPairCount)
	require.Equal(t, at, *observation.LastSeenAt)
	require.Empty(t, preview.Warnings)
	require.Equal(t, before.UserAgent, preview.Persona.UserAgent)
	require.Equal(t, before.Version, preview.Persona.Version)
	// A minimum version changes the generated identity; don't claim the input
	// alpha pair proves that the resulting stable version was observed.
	preview, err = PreviewCodexUserAgentConfig(config, "0.155.0", nil)
	require.NoError(t, err)
	index.Apply(&preview)
	require.Equal(t, "unseen", preview.Persona.Observation.Status)
	require.Zero(t, preview.Persona.Observation.VersionPairCount)
	require.Contains(t, preview.Warnings, "version_pair")
}

func TestCodexUAObservationsKeepVersionPairsAndClientsTogether(t *testing.T) {
	at := time.Now().UTC()
	vs := "codex_vscode/0.154.0-alpha.6.2 (Windows 10.0.26200; x86_64) unknown (VS Code; 26.908.40401)"
	exec := "codex_exec/0.155.0-alpha.9 (Windows 10.0.26200; x86_64) unknown (codex_exec; 0.155.0-alpha.9)"
	index := NewCodexUserAgentObservations([]database.UsageClientUserAgentSample{
		{UserAgent: vs, Count: 2, LastSeen: at},
		{UserAgent: strings.ReplaceAll(vs, "0.154.0-alpha.6.2", "0.155.0-alpha.9"), Count: 1, LastSeen: at},
		{UserAgent: exec, Count: 3, LastSeen: at},
	}, at)
	require.Equal(t, "matched", index.observe(vs).Status)
	require.Equal(t, "matched", index.observe(exec).Status)
	require.Equal(t, "empty", index.observe(observedDesktopUA).Status)
	for _, unknown := range []string{
		strings.ReplaceAll(vs, "26.908.40401", "26.915.31029"),
		strings.ReplaceAll(vs, "VS Code;", "Cursor;"),
		strings.ReplaceAll(exec, "codex_exec; 0.155.0-alpha.9", "codex_exec; 26.915.31029"),
	} {
		require.Contains(t, index.observe(unknown).Warnings, "version_pair")
	}
}

func TestCodexUAObservationsDoNotInventCompleteCombinations(t *testing.T) {
	at := time.Now().UTC()
	mac := strings.ReplaceAll(strings.ReplaceAll(observedDesktopUA, "Windows 10.0.26200", "Mac OS 15.5.0"), " unknown ", " WindowsTerminal ")
	index := NewCodexUserAgentObservations([]database.UsageClientUserAgentSample{
		{UserAgent: observedDesktopUA, Count: 2, LastSeen: at},
		{UserAgent: mac, Count: 3, LastSeen: at},
	}, at)
	combined := strings.ReplaceAll(observedDesktopUA, " unknown ", " WindowsTerminal ")
	got := index.observe(combined)
	require.Equal(t, "unseen", got.Status)
	require.Equal(t, []string{"combination"}, got.Warnings)
	require.EqualValues(t, 5, got.VersionPairCount)
	require.Zero(t, got.MatchCount)
}

func TestCodexUAObservationsDistinguishUnavailableEmptyAndMalformed(t *testing.T) {
	var absent *CodexUserAgentObservations
	require.Equal(t, "unavailable", absent.observe(observedDesktopUA).Status)
	index := NewCodexUserAgentObservations([]database.UsageClientUserAgentSample{{UserAgent: "broken\nUA", Count: 3}}, time.Now())
	require.Equal(t, "empty", index.observe(observedDesktopUA).Status)
	require.Empty(t, index.observe(observedDesktopUA).Warnings)
	require.Equal(t, "unparseable", index.observe("custom-client/1.0").Status)
	pool, err := PreviewCodexUserAgentConfig(`{"mode":"pool"}`, "", []int64{1, 2})
	require.NoError(t, err)
	index.Apply(&pool)
	for _, sample := range pool.Samples {
		require.Equal(t, "empty", sample.Observation.Status)
	}
}
