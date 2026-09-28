package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestSettingsExportCanBeImportedWithoutReplacingAdminOrRules(t *testing.T) {
	resetAntigravityOAuthSettingsEnv(t)
	// Imports carry image_storage_* fields; keep the local backend in a writable dir.
	t.Setenv("IMAGE_ASSET_DIR", t.TempDir())
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	db := newTestAdminDB(t)
	memory := cache.NewMemory(4)
	t.Cleanup(func() { _ = memory.Close() })
	ctx := context.Background()
	s := defaultBootstrapSettings()
	s.AdminSecret = "source-admin-test-only"
	s.GithubToken = "source-service-test-only"
	s.PromptFilterReviewAPIKey = "source-review-test-only"
	s.GlobalRPM = 321
	if err := db.UpdateSystemSettings(ctx, s); err != nil {
		t.Fatalf("UpdateSystemSettings: %v", err)
	}
	oauth := auth.AntigravityOAuthSettings{ActiveKey: "migrate", Clients: []auth.AntigravityOAuthClientConfig{{Key: "migrate", ClientID: "source-client-test-only", ClientSecret: "source-client-secret-test-only"}}}
	oauthRaw, err := auth.EncodeAntigravityOAuthSettings(oauth)
	if err != nil {
		t.Fatalf("EncodeAntigravityOAuthSettings: %v", err)
	}
	if err := db.SaveAntigravityOAuthConfig(ctx, oauthRaw); err != nil {
		t.Fatalf("SaveAntigravityOAuthConfig: %v", err)
	}
	auth.SetConfiguredAntigravityOAuth(oauth)
	proxy.ApplyRuntimeSettingsFromSystem(s)
	store := auth.NewStore(db, memory, s)
	t.Cleanup(store.Stop)
	h := NewHandler(store, db, memory, proxy.NewRateLimiter(s.GlobalRPM), "admin-secret")

	r := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(r)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/settings/export", nil)
	h.ExportSettings(c)
	if r.Code != http.StatusOK {
		t.Fatalf("export status = %d: %s", r.Code, r.Body.String())
	}
	if got := r.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	var backup struct {
		Format   string                     `json:"format"`
		Version  int                        `json:"version"`
		Settings map[string]json.RawMessage `json:"settings"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &backup); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	if backup.Format != "codex2api.settings" || backup.Version != 1 {
		t.Fatalf("format/version = %q/%d", backup.Format, backup.Version)
	}
	for _, field := range []string{"admin_secret", "admin_auth_source", "response_cache_config_generation", "prompt_filter_custom_patterns", "database_driver", "codex_effective_cli_version"} {
		if _, ok := backup.Settings[field]; ok {
			t.Fatalf("export must not contain %q", field)
		}
	}
	if got := string(backup.Settings["github_token"]); got != `"source-service-test-only"` {
		t.Fatalf("github_token = %s", got)
	}
	if got := string(backup.Settings["prompt_filter_review_api_key"]); got != `"source-review-test-only"` {
		t.Fatalf("prompt_filter_review_api_key = %s", got)
	}
	clients := string(backup.Settings["antigravity_oauth_clients"])
	if !strings.Contains(clients, "source-client-secret-test-only") || strings.Contains(clients, "has_secret") {
		t.Fatalf("antigravity_oauth_clients = %s", clients)
	}
	if got := string(backup.Settings["global_rpm"]); got != "321" {
		t.Fatalf("global_rpm = %s", got)
	}
	for key, value := range backup.Settings {
		if string(value) == "null" {
			t.Fatalf("%s exported as null", key)
		}
	}

	// Import through the real update endpoint, including its validation and SQL paths.
	s.AdminSecret = "target-admin-test-only"
	s.GlobalRPM = 42
	s.GithubToken = ""
	if err := db.SaveAntigravityOAuthConfig(ctx, "{}"); err != nil {
		t.Fatalf("reset OAuth config: %v", err)
	}
	auth.SetConfiguredAntigravityOAuth(auth.AntigravityOAuthSettings{})
	if err := db.UpdateSystemSettings(ctx, s); err != nil {
		t.Fatalf("UpdateSystemSettings target: %v", err)
	}
	encoded, err := json.Marshal(backup.Settings)
	if err != nil {
		t.Fatalf("encode import: %v", err)
	}
	r = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(r)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", bytes.NewReader(encoded))
	c.Request.Header.Set("Content-Type", "application/json")
	h.UpdateSettings(c)
	if r.Code != http.StatusOK {
		t.Fatalf("import status = %d: %s", r.Code, r.Body.String())
	}
	persisted, err := db.GetSystemSettings(ctx)
	if err != nil {
		t.Fatalf("GetSystemSettings: %v", err)
	}
	if persisted.AdminSecret != "target-admin-test-only" {
		t.Fatalf("import replaced the target admin login key: %q", persisted.AdminSecret)
	}
	if persisted.PromptFilterCustomPatterns != s.PromptFilterCustomPatterns {
		t.Fatal("import must not replace custom prompt rules")
	}
	if persisted.GlobalRPM != 321 || persisted.GithubToken != "source-service-test-only" {
		t.Fatalf("imported values not applied: rpm=%d github=%q", persisted.GlobalRPM, persisted.GithubToken)
	}
	restoredRaw, err := db.LoadAntigravityOAuthConfig(ctx)
	if err != nil {
		t.Fatalf("LoadAntigravityOAuthConfig: %v", err)
	}
	restored, err := auth.ParseAntigravityOAuthSettings(restoredRaw)
	if err != nil {
		t.Fatalf("ParseAntigravityOAuthSettings: %v", err)
	}
	if !reflect.DeepEqual(restored, oauth) {
		t.Fatalf("OAuth clients = %+v, want %+v", restored, oauth)
	}
}
