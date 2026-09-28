package admin

import (
	"database/sql"
	"net/http"
	"testing"

	"github.com/codex2api/proxy"
	"github.com/stretchr/testify/require"
)

func TestRateLimitRetryPolicyIndependentSettingsAndReload(t *testing.T) {
	h, db, _ := newImagesSettingsHandler(t)
	for _, tc := range []struct {
		patch           map[string]any
		rate, transport string
		account         int
	}{
		{map[string]any{"rate_limit_retry_policy": "sticky", "bps_attachment_account_concurrency": 20}, "sticky", "rotate", 20},
		{map[string]any{"transport_retry_policy": "sticky"}, "sticky", "sticky", 20},
		{map[string]any{"rate_limit_retry_policy": "off"}, "off", "sticky", 20},
		{map[string]any{"transport_retry_policy": "rotate"}, "off", "rotate", 20},
		{map[string]any{"site_name": "independent", "rate_limit_retry_policy": nil}, "off", "rotate", 20},
		{map[string]any{"rate_limit_retry_policy": "rotate", "bps_attachment_account_concurrency": 15}, "rotate", "rotate", 15},
	} {
		proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings())
		w := invokeResponseCacheSettingsAdmin(t, h, http.MethodPut, tc.patch)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		got := decodeResponseCacheSettingsResponse(t, w)
		require.Equal(t, tc.rate, got.RateLimitRetryPolicy)
		require.Equal(t, tc.transport, got.TransportRetryPolicy)
		require.Equal(t, tc.account, got.BPSAttachmentAccountConcurrency)
		s, err := db.GetSystemSettings(t.Context())
		require.NoError(t, err)
		require.Equal(t, tc.rate, s.RateLimitRetryPolicy)
		require.Equal(t, tc.account, s.BPSAttachmentAccountConcurrency)
		cfg := proxy.ApplyRuntimeSettingsFromSystem(s)
		require.Equal(t, tc.rate, cfg.RateLimitRetryPolicy)
		require.Equal(t, tc.account, cfg.BPSAttachmentAccountConcurrency)
	}
	for _, patch := range []map[string]any{{"rate_limit_retry_policy": "unknown"}, {"rate_limit_retry_policy": true}, {"bps_attachment_account_concurrency": 0}, {"bps_attachment_account_concurrency": 1025}} {
		w := invokeResponseCacheSettingsAdmin(t, h, http.MethodPut, patch)
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	}
}

func TestRateLimitRetryAndAccountUploadSaveFailure(t *testing.T) {
	h, db, path := newImagesSettingsHandler(t)
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer raw.Close()
	_, err = raw.Exec(`CREATE TRIGGER reject_rate_settings BEFORE INSERT ON system_settings BEGIN SELECT RAISE(ABORT, 'forced failure'); END`)
	require.NoError(t, err)
	for _, patch := range []map[string]any{{"rate_limit_retry_policy": "off"}, {"bps_attachment_account_concurrency": 23}} {
		w := invokeResponseCacheSettingsAdmin(t, h, http.MethodPut, patch)
		require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
		cfg := proxy.CurrentRuntimeSettings()
		require.Equal(t, "rotate", cfg.RateLimitRetryPolicy)
		require.Equal(t, 15, cfg.BPSAttachmentAccountConcurrency)
		s, err := db.GetSystemSettings(t.Context())
		require.NoError(t, err)
		require.Equal(t, "rotate", s.RateLimitRetryPolicy)
		require.Equal(t, 15, s.BPSAttachmentAccountConcurrency)
	}
}
