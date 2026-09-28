package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSettingsExportCanBeImportedWithoutReplacingAdminOrRules(t *testing.T) {
	resetAntigravityOAuthSettingsEnv(t)
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	db := newTestAdminDB(t)
	memory := cache.NewMemory(4)
	t.Cleanup(func() { _ = memory.Close() })
	s := defaultBootstrapSettings()
	s.AdminSecret = "source-admin-test-only"
	s.GithubToken = "source-service-test-only"
	s.PromptFilterReviewAPIKey = "source-review-test-only"
	s.CodexInitialSessionMaxAgeSeconds = 123
	s.CodexInitialSessionAgeCheckDisabled = true
	s.BPSAttachmentRequestConcurrency, s.BPSAttachmentInstanceConcurrency, s.ResinAccountMaxConns = 9, 96, 20
	require.NoError(t, db.UpdateSystemSettings(context.Background(), s))
	oauth := auth.AntigravityOAuthSettings{ActiveKey: "migrate", Clients: []auth.AntigravityOAuthClientConfig{{Key: "migrate", ClientID: "source-client-test-only", ClientSecret: "source-client-secret-test-only"}}}
	oauthRaw, err := auth.EncodeAntigravityOAuthSettings(oauth)
	require.NoError(t, err)
	require.NoError(t, db.SaveAntigravityOAuthConfig(context.Background(), oauthRaw))
	auth.SetConfiguredAntigravityOAuth(oauth)
	proxy.ApplyRuntimeSettingsFromSystem(s)
	store := auth.NewStore(db, memory, s)
	t.Cleanup(store.Stop)
	h := NewHandler(store, db, memory, proxy.NewRateLimiter(s.GlobalRPM), "admin-secret")
	r := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(r)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/settings/export", nil)
	h.ExportSettings(c)
	require.Equal(t, http.StatusOK, r.Code, r.Body.String())
	require.Equal(t, "no-store", r.Header().Get("Cache-Control"))
	var backup struct {
		Format   string                     `json:"format"`
		Version  int                        `json:"version"`
		Settings map[string]json.RawMessage `json:"settings"`
	}
	require.NoError(t, json.Unmarshal(r.Body.Bytes(), &backup))
	require.Equal(t, "codex2api.settings", backup.Format)
	require.Equal(t, 1, backup.Version)
	for _, field := range []string{"admin_secret", "admin_auth_source", "response_cache_config_generation", "prompt_filter_custom_patterns", "database_driver", "codex_effective_cli_version"} {
		require.NotContains(t, backup.Settings, field)
	}
	require.Equal(t, `true`, string(backup.Settings["codex_initial_session_age_check_disabled"]))
	require.Equal(t, `"source-service-test-only"`, string(backup.Settings["github_token"]))
	require.Equal(t, `"source-review-test-only"`, string(backup.Settings["prompt_filter_review_api_key"]))
	require.Contains(t, string(backup.Settings["antigravity_oauth_clients"]), "source-client-secret-test-only")
	require.NotContains(t, string(backup.Settings["antigravity_oauth_clients"]), "has_secret")
	for key, value := range backup.Settings {
		require.NotEqual(t, "null", string(value), key)
	}
	// Use the real update endpoint, including its full validation and SQL paths.
	s.AdminSecret = "target-admin-test-only"
	s.CodexInitialSessionAgeCheckDisabled = false
	s.CodexInitialSessionMaxAgeSeconds = 42
	s.BPSAttachmentRequestConcurrency, s.BPSAttachmentInstanceConcurrency, s.ResinAccountMaxConns = 15, 64, 15
	require.NoError(t, db.SaveAntigravityOAuthConfig(context.Background(), "{}"))
	auth.SetConfiguredAntigravityOAuth(auth.AntigravityOAuthSettings{})
	require.NoError(t, db.UpdateSystemSettings(context.Background(), s))
	encoded, err := json.Marshal(backup.Settings)
	require.NoError(t, err)
	r = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(r)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", bytes.NewReader(encoded))
	c.Request.Header.Set("Content-Type", "application/json")
	h.UpdateSettings(c)
	require.Equal(t, http.StatusOK, r.Code, r.Body.String())
	persisted, err := db.GetSystemSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, "target-admin-test-only", persisted.AdminSecret)
	require.Equal(t, s.PromptFilterCustomPatterns, persisted.PromptFilterCustomPatterns)
	require.True(t, persisted.CodexInitialSessionAgeCheckDisabled)
	require.Equal(t, 123, persisted.CodexInitialSessionMaxAgeSeconds)
	require.Equal(t, 9, persisted.BPSAttachmentRequestConcurrency)
	require.Equal(t, 96, persisted.BPSAttachmentInstanceConcurrency)
	require.Equal(t, 20, persisted.ResinAccountMaxConns)
	require.True(t, proxy.CurrentRuntimeSettings().CodexInitialSessionAgeCheckDisabled)
	restoredRaw, err := db.LoadAntigravityOAuthConfig(context.Background())
	require.NoError(t, err)
	restored, err := auth.ParseAntigravityOAuthSettings(restoredRaw)
	require.NoError(t, err)
	require.Equal(t, oauth, restored)
	// Unrelated writes retain disabled state, even if the local snapshot is stale.
	for _, payload := range []string{`{"site_name":"migrated"}`, `{"codex_initial_session_age_check_disabled":false}`} {
		proxy.UpdateRuntimeSettings(func(current proxy.RuntimeSettings) proxy.RuntimeSettings {
			current.CodexInitialSessionAgeCheckDisabled = false
			return current
		})
		r = httptest.NewRecorder()
		c, _ = gin.CreateTestContext(r)
		c.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(payload))
		c.Request.Header.Set("Content-Type", "application/json")
		h.UpdateSettings(c)
		require.Equal(t, http.StatusOK, r.Code, r.Body.String())
		persisted, err = db.GetSystemSettings(context.Background())
		require.NoError(t, err)
		require.Equal(t, 123, persisted.CodexInitialSessionMaxAgeSeconds)
		require.Equal(t, !strings.Contains(payload, "false"), persisted.CodexInitialSessionAgeCheckDisabled)
	}
}
