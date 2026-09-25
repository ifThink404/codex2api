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

func TestSessionBalanceSettingsUpdateAndExport(t *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	db := newTestAdminDB(t)
	memoryCache := cache.NewMemory(4)
	t.Cleanup(func() { _ = memoryCache.Close() })
	settings := defaultBootstrapSettings()
	if err := db.UpdateSystemSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, memoryCache, settings)
	t.Cleanup(store.Stop)
	handler := NewHandler(store, db, memoryCache, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret")
	for _, step := range []struct {
		name, patch, want string
		status            int
	}{
		{"default", "", "default", 200},
		{"slots", `{"session_balance_mode":"window"}`, "window", 200},
		{"unrelated update", `{"site_name":"balance"}`, "window", 200},
		{"sessions", `{"session_balance_mode":"session"}`, "session", 200},
		{"modern overrides legacy", `{"session_balance_mode":"default","session_window_balance_enabled":true}`, "default", 200},
		{"old import enabled", `{"session_window_balance_enabled":true}`, "session", 200},
		{"old import disabled", `{"session_window_balance_enabled":false}`, "default", 200},
		{"normalized", `{"session_balance_mode":" WINDOW "}`, "window", 200},
		{"invalid mode", `{"session_balance_mode":"random","session_window_balance_enabled":false}`, "window", 400},
		{"empty mode", `{"session_balance_mode":""}`, "window", 400},
	} {
		t.Run(step.name, func(t *testing.T) {
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
				t.Fatalf("status %d: %s", recorder.Code, recorder.Body.String())
			}
			if step.status == 200 {
				var got settingsResponse
				if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if got.SessionBalanceMode != step.want {
					t.Fatalf("response mode %q, want %q", got.SessionBalanceMode, step.want)
				}
			}
			persisted, err := db.GetSystemSettings(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if store.GetSessionBalanceMode() != step.want || persisted.SessionBalanceMode != step.want {
				t.Fatalf("runtime/storage mode mismatch: %q / %q", store.GetSessionBalanceMode(), persisted.SessionBalanceMode)
			}
			// Export contract is also the import payload used by the settings UI.
			snapshot, err := handler.settingsSnapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			portable, err := portableSettings(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if string(portable["session_balance_mode"]) != `"`+step.want+`"` {
				t.Fatalf("export lost mode: %s", portable["session_balance_mode"])
			}
		})
	}
}
