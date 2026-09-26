package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestBPSTurnSettingsPersistAndValidate(t *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	db := newTestAdminDB(t)
	memory := cache.NewMemory(4)
	t.Cleanup(func() { _ = memory.Close() })
	settings := defaultBootstrapSettings()
	require.NoError(t, db.UpdateSystemSettings(t.Context(), settings))
	proxy.ApplyRuntimeSettingsFromSystem(settings)
	store := auth.NewStore(db, memory, settings)
	t.Cleanup(store.Stop)
	h := NewHandler(store, db, memory, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret")
	for _, tc := range []struct {
		name, patch  string
		want, status int
		stale        bool
	}{
		{"default", "", 24, 200, false},
		{"set_sixth_mode", `{"bps_turn_task_lifetime_hours":36,"codex_fingerprint_default_mode":"turn_round"}`, 36, 200, false},
		{"get", "", 36, 200, false},
		{"unrelated_save_preserves_lifetime", `{"site_name":"Turn rounds"}`, 36, 200, true},
		{"null_omits", `{"bps_turn_task_lifetime_hours":null}`, 36, 200, false},
		{"reject_zero", `{"bps_turn_task_lifetime_hours":0}`, 36, 400, false},
		{"reject_negative", `{"bps_turn_task_lifetime_hours":-1}`, 36, 400, false},
		{"reject_huge", `{"bps_turn_task_lifetime_hours":8761}`, 36, 400, false},
		{"reject_fraction", `{"bps_turn_task_lifetime_hours":2.5}`, 36, 400, false},
		{"one_hour", `{"bps_turn_task_lifetime_hours":1}`, 1, 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.stale {
				proxy.UpdateRuntimeSettings(func(s proxy.RuntimeSettings) proxy.RuntimeSettings { s.BPSTurnTaskLifetimeHours = 99; return s })
			}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			if tc.patch == "" {
				c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil)
				h.GetSettings(c)
			} else {
				c.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(tc.patch))
				c.Request.Header.Set("Content-Type", "application/json")
				h.UpdateSettings(c)
			}
			require.Equal(t, tc.status, w.Code, w.Body.String())
			if tc.status == 200 {
				require.EqualValues(t, tc.want, gjson.Get(w.Body.String(), "bps_turn_task_lifetime_hours").Int())
			}
			require.Equal(t, tc.want, proxy.CurrentRuntimeSettings().BPSTurnTaskLifetimeHours)
			persisted, err := db.GetSystemSettings(t.Context())
			require.NoError(t, err)
			require.Equal(t, tc.want, persisted.BPSTurnTaskLifetimeHours)
			if tc.name != "default" {
				require.Equal(t, "turn_round", persisted.CodexFingerprintDefaultMode)
				require.Equal(t, "turn_round", store.GetCodexFingerprintDefaultMode())
			}
			proxy.ApplyRuntimeSettingsFromSystem(persisted)
			require.Equal(t, tc.want, proxy.CurrentRuntimeSettings().BPSTurnTaskLifetimeHours)
		})
	}
}

func TestBPSTurnQuestionLimitSettings(t *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	db := newTestAdminDB(t)
	memory := cache.NewMemory(4)
	t.Cleanup(func() { _ = memory.Close() })
	settings := defaultBootstrapSettings()
	settings.BPSRoundConvergenceLimit = 250
	settings.BPSTurnTaskLifetimeHours = 36
	require.NoError(t, db.UpdateSystemSettings(t.Context(), settings))
	proxy.ApplyRuntimeSettingsFromSystem(settings)
	store := auth.NewStore(db, memory, settings)
	t.Cleanup(store.Stop)
	h := NewHandler(store, db, memory, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret")
	for _, tc := range []struct {
		name, patch  string
		want, status int
	}{
		{"default", "", 100, 200},
		{"set", `{"bps_turn_round_limit":2}`, 2, 200},
		{"read", "", 2, 200},
		{"unrelated", `{"site_name":"User question limits"}`, 2, 200},
		{"null", `{"bps_turn_round_limit":null}`, 2, 200},
		{"zero", `{"bps_turn_round_limit":0}`, 2, 400},
		{"negative", `{"bps_turn_round_limit":-1}`, 2, 400},
		{"fraction", `{"bps_turn_round_limit":1.5}`, 2, 400},
		{"overflow", `{"bps_turn_round_limit":1000001}`, 2, 400},
		{"maximum", `{"bps_turn_round_limit":1000000}`, 1000000, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "unrelated" {
				proxy.UpdateRuntimeSettings(func(s proxy.RuntimeSettings) proxy.RuntimeSettings { s.BPSTurnRoundLimit = 99; return s })
			}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			if tc.patch == "" {
				c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil)
				h.GetSettings(c)
			} else {
				c.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(tc.patch))
				c.Request.Header.Set("Content-Type", "application/json")
				h.UpdateSettings(c)
			}
			require.Equal(t, tc.status, w.Code, w.Body.String())
			if tc.status == 200 {
				require.EqualValues(t, tc.want, gjson.Get(w.Body.String(), "bps_turn_round_limit").Int())
			}
			require.Equal(t, tc.want, proxy.CurrentRuntimeSettings().BPSTurnRoundLimit)
			persisted, err := db.GetSystemSettings(t.Context())
			require.NoError(t, err)
			require.Equal(t, tc.want, persisted.BPSTurnRoundLimit)
			require.Equal(t, 250, persisted.BPSRoundConvergenceLimit)
			require.Equal(t, 36, persisted.BPSTurnTaskLifetimeHours)
			proxy.ApplyRuntimeSettingsFromSystem(persisted)
			require.Equal(t, tc.want, proxy.CurrentRuntimeSettings().BPSTurnRoundLimit)
		})
	}
}
