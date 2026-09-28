package admin

import (
	"database/sql"
	"net/http"
	"testing"

	"github.com/codex2api/proxy"
	"github.com/stretchr/testify/require"
)

func TestAttachmentConcurrencySettingsPartialUpdateReloadAndValidation(t *testing.T) {
	h, db, _ := newImagesSettingsHandler(t)
	for _, tc := range []struct {
		patch map[string]any
		want  [3]int
	}{
		{map[string]any{"bps_attachment_request_concurrency": 12}, [3]int{12, 64, 15}},
		{map[string]any{"bps_attachment_instance_concurrency": 128}, [3]int{12, 128, 15}},
		{map[string]any{"resin_account_max_conns": 24}, [3]int{12, 128, 24}},
		{map[string]any{"site_name": "Preserve saved upload limits"}, [3]int{12, 128, 24}},
		{map[string]any{"bps_attachment_request_concurrency": nil}, [3]int{12, 128, 24}},
		{map[string]any{"bps_attachment_request_concurrency": 64, "bps_attachment_instance_concurrency": 1, "resin_account_max_conns": 1}, [3]int{64, 1, 1}},
	} {
		// Unrelated writes must use the database even when this replica is stale.
		proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings())
		response := invokeResponseCacheSettingsAdmin(t, h, http.MethodPut, tc.patch)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		got := decodeResponseCacheSettingsResponse(t, response)
		require.Equal(t, tc.want, [3]int{got.BPSAttachmentRequestConcurrency, got.BPSAttachmentInstanceConcurrency, got.ResinAccountMaxConns})
		persisted, err := db.GetSystemSettings(t.Context())
		require.NoError(t, err)
		require.Equal(t, tc.want, [3]int{persisted.BPSAttachmentRequestConcurrency, persisted.BPSAttachmentInstanceConcurrency, persisted.ResinAccountMaxConns})
		proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings())
		cfg := proxy.ApplyRuntimeSettingsFromSystem(persisted)
		require.Equal(t, tc.want, [3]int{cfg.BPSAttachmentRequestConcurrency, cfg.BPSAttachmentInstanceConcurrency, cfg.ResinAccountMaxConns})
		get := invokeResponseCacheSettingsAdmin(t, h, http.MethodGet, nil)
		require.Equal(t, http.StatusOK, get.Code, get.Body.String())
		got = decodeResponseCacheSettingsResponse(t, get)
		require.Equal(t, tc.want, [3]int{got.BPSAttachmentRequestConcurrency, got.BPSAttachmentInstanceConcurrency, got.ResinAccountMaxConns})
	}
	before := proxy.CurrentRuntimeSettings()
	for _, field := range []string{"bps_attachment_request_concurrency", "bps_attachment_instance_concurrency", "resin_account_max_conns"} {
		for _, invalid := range []any{0, -1, 1025, 1.5, "15"} {
			response := invokeResponseCacheSettingsAdmin(t, h, http.MethodPut, map[string]any{field: invalid})
			require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
			require.Equal(t, before, proxy.CurrentRuntimeSettings())
		}
	}
	response := invokeResponseCacheSettingsAdmin(t, h, http.MethodPut, map[string]any{"bps_attachment_request_concurrency": 65})
	require.Equal(t, http.StatusBadRequest, response.Code)
}

func TestAttachmentConcurrencySettingsPersistenceFailureKeepsRuntime(t *testing.T) {
	h, db, path := newImagesSettingsHandler(t)
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer raw.Close()
	_, err = raw.Exec(`CREATE TRIGGER reject_attachment_settings BEFORE INSERT ON system_settings BEGIN SELECT RAISE(ABORT, 'forced settings write failure'); END`)
	require.NoError(t, err)
	response := invokeResponseCacheSettingsAdmin(t, h, http.MethodPut, map[string]any{
		"bps_attachment_request_concurrency": 8, "bps_attachment_instance_concurrency": 128, "resin_account_max_conns": 30,
	})
	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	cfg := proxy.CurrentRuntimeSettings()
	require.Equal(t, [3]int{15, 64, 15}, [3]int{cfg.BPSAttachmentRequestConcurrency, cfg.BPSAttachmentInstanceConcurrency, cfg.ResinAccountMaxConns})
	persisted, err := db.GetSystemSettings(t.Context())
	require.NoError(t, err)
	require.Equal(t, [3]int{15, 64, 15}, [3]int{persisted.BPSAttachmentRequestConcurrency, persisted.BPSAttachmentInstanceConcurrency, persisted.ResinAccountMaxConns})
}
