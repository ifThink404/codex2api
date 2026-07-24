package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/proxy"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

func TestUpdateSettingsRejectsBroadCustomRuleWithoutChangingPersistenceOrStore(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	settings := defaultBootstrapSettings()
	settings.PromptFilterCustomPatterns = `[{
		"name":"existing_safe_rule",
		"pattern":"terminal-safe-marker",
		"weight":60,
		"category":"custom"
	}]`
	if err := db.UpdateSystemSettings(context.Background(), settings); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	store := auth.NewStore(db, tc, settings)
	t.Cleanup(store.Stop)
	handler := NewHandler(store, db, tc, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret")

	before := promptfilter.MarshalCustomPatterns(store.GetPromptFilterConfig().CustomPatterns)
	submitted := []promptfilter.PatternConfig{{Name: "all", Pattern: `(?i)\ball\b`, Weight: 100, Strict: true}}
	submittedJSON, _ := json.Marshal(submitted)
	body, _ := json.Marshal(map[string]string{"prompt_filter_custom_patterns": string(submittedJSON)})
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.UpdateSettings(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 body=%s", recorder.Code, recorder.Body.String())
	}
	if got := promptfilter.MarshalCustomPatterns(store.GetPromptFilterConfig().CustomPatterns); got != before {
		t.Fatalf("runtime rules changed\nbefore=%s\nafter=%s", before, got)
	}
	persisted, err := db.GetSystemSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if persisted.PromptFilterCustomPatterns != settings.PromptFilterCustomPatterns {
		t.Fatalf("persisted rules changed\nbefore=%s\nafter=%s", settings.PromptFilterCustomPatterns, persisted.PromptFilterCustomPatterns)
	}
}

func TestUpdateSettingsAllowsExplicitlyDisabledBroadLegacyRule(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	settings := defaultBootstrapSettings()
	if err := db.UpdateSystemSettings(context.Background(), settings); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	store := auth.NewStore(db, tc, settings)
	t.Cleanup(store.Stop)
	handler := NewHandler(store, db, tc, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret")

	disabled := false
	submitted := []promptfilter.PatternConfig{{Name: "all", Pattern: `(?i)\ball\b`, Weight: 100, Strict: true, Enabled: &disabled}}
	submittedJSON, _ := json.Marshal(submitted)
	body, _ := json.Marshal(map[string]string{"prompt_filter_custom_patterns": string(submittedJSON)})
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.UpdateSettings(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 body=%s", recorder.Code, recorder.Body.String())
	}
	got := store.GetPromptFilterConfig().CustomPatterns
	if len(got) != 1 || got[0].Enabled == nil || *got[0].Enabled {
		t.Fatalf("disabled legacy rule was not retained: %#v", got)
	}
}
