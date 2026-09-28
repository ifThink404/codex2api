package admin

import (
	"context"
	"database/sql"
	"net/http"
	"testing"

	"github.com/codex2api/proxy"
	"github.com/stretchr/testify/require"
)

func TestEarlySSESettingsPartialUpdatesAndReload(t *testing.T) {
	h, db, _ := newImagesSettingsHandler(t)
	require.False(t, defaultBootstrapSettings().CodexEarlySSEPassthroughEnabled)
	require.False(t, proxy.CurrentRuntimeSettings().CodexEarlySSEPassthrough)
	for _, tc := range []struct {
		patch         map[string]any
		early, report bool
	}{
		{map[string]any{"codex_preflight_sse_passthrough_enabled": true}, false, true},
		{map[string]any{"codex_early_sse_passthrough_enabled": true}, true, true},
		{map[string]any{"site_name": "Keep early SSE preference"}, true, true},
		{map[string]any{"codex_preflight_sse_passthrough_enabled": false}, true, false},
		{map[string]any{"continuous_retry_enabled": true}, true, false},
		{map[string]any{"codex_early_sse_passthrough_enabled": false}, false, false},
	} {
		response := invokeResponseCacheSettingsAdmin(t, h, http.MethodPut, tc.patch)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		got := decodeResponseCacheSettingsResponse(t, response)
		require.Equal(t, tc.early, got.CodexEarlySSEPassthroughEnabled)
		require.Equal(t, tc.report, got.CodexPreflightSSEPassthroughEnabled)
		require.Equal(t, tc.early, proxy.CurrentRuntimeSettings().CodexEarlySSEPassthrough)
		persisted, err := db.GetSystemSettings(context.Background())
		require.NoError(t, err)
		require.Equal(t, tc.early, persisted.CodexEarlySSEPassthroughEnabled)
		require.Equal(t, tc.report, persisted.CodexPreflightSSEPassthroughEnabled)
		proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings())
		proxy.ApplyRuntimeSettingsFromSystem(persisted)
		require.Equal(t, tc.early, proxy.CurrentRuntimeSettings().CodexEarlySSEPassthrough)
		get := invokeResponseCacheSettingsAdmin(t, h, http.MethodGet, nil)
		require.Equal(t, http.StatusOK, get.Code, get.Body.String())
		require.Equal(t, tc.early, decodeResponseCacheSettingsResponse(t, get).CodexEarlySSEPassthroughEnabled)
	}
}

func TestEarlySSESettingsPersistenceFailureKeepsRuntime(t *testing.T) {
	h, db, path := newImagesSettingsHandler(t)
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer raw.Close()
	_, err = raw.Exec(`CREATE TRIGGER reject_early_sse_settings BEFORE INSERT ON system_settings BEGIN SELECT RAISE(ABORT, 'forced settings write failure'); END`)
	require.NoError(t, err)
	response := invokeResponseCacheSettingsAdmin(t, h, http.MethodPut, map[string]any{"codex_early_sse_passthrough_enabled": true})
	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	require.False(t, proxy.CurrentRuntimeSettings().CodexEarlySSEPassthrough)
	persisted, err := db.GetSystemSettings(context.Background())
	require.NoError(t, err)
	require.False(t, persisted.CodexEarlySSEPassthroughEnabled)
}
