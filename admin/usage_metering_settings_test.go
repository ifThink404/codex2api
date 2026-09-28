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

func TestUsageMeteringSettingsPartialUpdates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	db := newTestAdminDB(t)
	tc := cache.NewMemory(4)
	t.Cleanup(func() { tc.Close() })
	settings := defaultBootstrapSettings()
	if !db.GetUsageMeteringEnabled() {
		t.Fatal("lightweight metering must default to enabled")
	}
	if err := db.UpdateSystemSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, tc, settings)
	t.Cleanup(store.Stop)
	handler := NewHandler(store, db, tc, proxy.NewRateLimiter(settings.GlobalRPM), "test-admin")
	for _, test := range []struct {
		body    string
		enabled bool
	}{
		{`{"usage_metering_enabled":false,"usage_log_mode":"off"}`, false},
		{`{"site_name":"Keep metering preference"}`, false},
		{`{"usage_metering_enabled":true}`, true},
		{`{"usage_log_mode":"errors"}`, true},
	} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(test.body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		handler.UpdateSettings(ctx)
		if recorder.Code != http.StatusOK {
			t.Fatalf("update: %d %s", recorder.Code, recorder.Body.String())
		}
		var response struct {
			Enabled *bool `json:"usage_metering_enabled"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Enabled == nil || *response.Enabled != test.enabled || db.GetUsageMeteringEnabled() != test.enabled {
			t.Fatalf("runtime/response mismatch: %s", recorder.Body.String())
		}
		if mode := db.GetUsageLogMode(); strings.Contains(test.body, "usage_log_mode") && !strings.Contains(test.body, mode) {
			t.Fatalf("usage_log_mode = %q after %s", mode, test.body)
		}
		recorder = httptest.NewRecorder()
		ctx, _ = gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil)
		handler.GetSettings(ctx)
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Enabled == nil || *response.Enabled != test.enabled {
			t.Fatalf("GET mismatch: %s", recorder.Body.String())
		}
	}
}
