package admin

import (
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
)

func TestCodexForkAccountFallbackSettingsRoundTrip(test *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	test.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	db := newTestAdminDB(test)
	memoryCache := cache.NewMemory(4)
	test.Cleanup(func() { _ = memoryCache.Close() })
	settings := defaultBootstrapSettings()
	if settings.CodexForkAccountFallbackEnabled {
		test.Fatal("bootstrap must leave session failover off")
	}
	if err := db.UpdateSystemSettings(context.Background(), settings); err != nil {
		test.Fatal(err)
	}
	proxy.ApplyRuntimeSettingsFromSystem(settings)
	store := auth.NewStore(db, memoryCache, settings)
	test.Cleanup(store.Stop)
	store.SetAPIKeyAllowedGroups(42, []int64{9})
	outsideGroup := &auth.Account{DBID: 123, GroupIDs: []int64{30}}
	handler := NewHandler(store, db, memoryCache, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret")
	for _, step := range []struct {
		name   string
		patch  string
		want   bool
		status int
		stale  bool
	}{
		{name: "default", status: http.StatusOK},
		{name: "enable", patch: `{"codex_fork_account_fallback_enabled":true}`, want: true, status: http.StatusOK},
		{name: "get enabled", want: true, status: http.StatusOK},
		{name: "omitted preserves persisted value", patch: `{"site_name":"Session failover test"}`, want: true, status: http.StatusOK, stale: true},
		{name: "null is omitted", patch: `{"codex_fork_account_fallback_enabled":null}`, want: true, status: http.StatusOK},
		{name: "invalid boolean", patch: `{"codex_fork_account_fallback_enabled":"true"}`, want: true, status: http.StatusBadRequest},
		{name: "disable", patch: `{"codex_fork_account_fallback_enabled":false}`, status: http.StatusOK},
		{name: "get disabled", status: http.StatusOK},
	} {
		test.Run(step.name, func(test *testing.T) {
			if step.stale {
				proxy.UpdateRuntimeSettings(func(current proxy.RuntimeSettings) proxy.RuntimeSettings {
					current.CodexForkAccountFallbackEnabled = !step.want
					return current
				})
			}
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			if step.patch == "" {
				ctx.Request = httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil)
				handler.GetSettings(ctx)
			} else {
				ctx.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(step.patch))
				ctx.Request.Header.Set("Content-Type", "application/json")
				handler.UpdateSettings(ctx)
			}
			if recorder.Code != step.status {
				test.Fatalf("settings status=%d want=%d body=%s", recorder.Code, step.status, recorder.Body.String())
			}
			if step.status == http.StatusOK {
				var response struct {
					Enabled *bool `json:"codex_fork_account_fallback_enabled"`
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
					test.Fatal(err)
				}
				if response.Enabled == nil || *response.Enabled != step.want {
					test.Fatalf("response toggle=%v want=%t", response.Enabled, step.want)
				}
			}
			if proxy.CurrentRuntimeSettings().CodexForkAccountFallbackEnabled != step.want {
				test.Fatal("runtime toggle does not match response")
			}
			if store.APIKeyAllowsAccount(42, outsideGroup) != step.want {
				test.Fatal("account group selection must follow the saved relaxed toggle")
			}
			persisted, err := db.GetSystemSettings(context.Background())
			if err != nil || persisted == nil {
				test.Fatalf("read persisted settings: %v", err)
			}
			if persisted.CodexForkAccountFallbackEnabled != step.want || persisted.CodexCapacityRetryEnabled != settings.CodexCapacityRetryEnabled || persisted.CodexOverloadPauseEnabled != settings.CodexOverloadPauseEnabled {
				test.Fatal("session failover did not persist independently of retry and overload settings")
			}
			proxy.ApplyRuntimeSettingsFromSystem(persisted)
			if proxy.CurrentRuntimeSettings().CodexForkAccountFallbackEnabled != step.want {
				test.Fatal("runtime reload lost the session failover toggle")
			}
		})
	}
}
