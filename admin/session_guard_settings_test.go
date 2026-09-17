package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// newSettingsTestHandler builds a Handler backed by a fresh SQLite DB and an
// in-memory token cache, mirroring the setup used by
// TestUpdateSettingsPersistsAutoResetCreditsAcrossPartialUpdates in
// handler_test.go.
func newSettingsTestHandler(t *testing.T) *Handler {
	t.Helper()

	db := newTestAdminDB(t)
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	settings := defaultBootstrapSettings()
	if err := db.UpdateSystemSettings(context.Background(), settings); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	store := auth.NewStore(db, tc, settings)
	t.Cleanup(store.Stop)
	proxy.ApplyRuntimeSettingsFromSystem(settings)
	return NewHandler(store, db, tc, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret")
}

func TestSessionGuardSettingsRoundtrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := newSettingsTestHandler(t) // reuse the existing helper from handler_test.go
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings()) })

	get := func() map[string]any {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil)
		h.GetSettings(c)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET settings = %d: %s", rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}
	put := func(body string) int {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.UpdateSettings(c)
		return rec.Code
	}

	initial := get()
	if initial["codex_turn_state_strict"] != false || initial["codex_session_no_borrow_enabled"] != false || initial["codex_initial_session_admission_enabled"] != false {
		t.Fatalf("switches must default off: %v", initial)
	}
	if initial["codex_session_no_borrow_hold_seconds"] != float64(20) || initial["codex_initial_session_max_age_seconds"] != float64(180) {
		t.Fatalf("numbers must default 20/180: %v", initial)
	}

	if code := put(`{"codex_turn_state_strict":true,"codex_session_no_borrow_enabled":true,"codex_session_no_borrow_hold_seconds":25,"codex_initial_session_admission_enabled":true,"codex_initial_session_max_age_seconds":600}`); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	after := get()
	if after["codex_turn_state_strict"] != true || after["codex_session_no_borrow_enabled"] != true || after["codex_initial_session_admission_enabled"] != true {
		t.Fatalf("switches did not persist: %v", after)
	}
	if after["codex_session_no_borrow_hold_seconds"] != float64(25) || after["codex_initial_session_max_age_seconds"] != float64(600) {
		t.Fatalf("numbers did not persist: %v", after)
	}
	runtime := proxy.CurrentRuntimeSettings()
	if !runtime.CodexTurnStateStrict || !runtime.CodexInitialSessionAdmissionEnabled || runtime.CodexInitialSessionMaxAgeSeconds != 600 {
		t.Fatalf("runtime settings not hot-applied: %#v", runtime)
	}
	if !h.store.SessionNoBorrowEnabled() || h.store.SessionNoBorrowHold() != 25*time.Second {
		t.Fatalf("store no-borrow not hot-applied: %v %s", h.store.SessionNoBorrowEnabled(), h.store.SessionNoBorrowHold())
	}

	if code := put(`{"codex_session_no_borrow_hold_seconds":31}`); code != http.StatusOK {
		t.Fatalf("PUT hold 31 = %d", code)
	}
	if get()["codex_session_no_borrow_hold_seconds"] != float64(30) {
		t.Fatal("hold must clamp to 30")
	}
	if code := put(`{"codex_initial_session_max_age_seconds":0}`); code != http.StatusOK {
		t.Fatalf("PUT max age 0 = %d", code)
	}
	if get()["codex_initial_session_max_age_seconds"] != float64(180) {
		t.Fatal("max age 0 must normalize to 180")
	}
}
