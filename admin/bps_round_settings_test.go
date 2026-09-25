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

func TestBPSRoundSettingsPersistAndValidate(t *testing.T) {
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
		{"default", "", 100, 200, false},
		{"set_fifth_mode", `{"bps_round_convergence_limit":3,"codex_fingerprint_default_mode":"round"}`, 3, 200, false},
		{"get", "", 3, 200, false},
		{"unrelated_save_preserves_limit", `{"site_name":"Round mode"}`, 3, 200, true},
		{"null_omits", `{"bps_round_convergence_limit":null}`, 3, 200, false},
		{"reject_zero", `{"bps_round_convergence_limit":0}`, 3, 400, false},
		{"reject_negative", `{"bps_round_convergence_limit":-1}`, 3, 400, false},
		{"reject_huge", `{"bps_round_convergence_limit":1000001}`, 3, 400, false},
		{"reject_fraction", `{"bps_round_convergence_limit":2.5}`, 3, 400, false},
		{"new_limit", `{"bps_round_convergence_limit":100}`, 100, 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.stale {
				proxy.UpdateRuntimeSettings(func(s proxy.RuntimeSettings) proxy.RuntimeSettings { s.BPSRoundConvergenceLimit = 99; return s })
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
				require.EqualValues(t, tc.want, gjson.Get(w.Body.String(), "bps_round_convergence_limit").Int())
			}
			require.Equal(t, tc.want, proxy.CurrentRuntimeSettings().BPSRoundConvergenceLimit)
			persisted, err := db.GetSystemSettings(t.Context())
			require.NoError(t, err)
			require.Equal(t, tc.want, persisted.BPSRoundConvergenceLimit)
			if tc.name != "default" {
				require.Equal(t, "round", persisted.CodexFingerprintDefaultMode)
			}
			proxy.ApplyRuntimeSettingsFromSystem(persisted)
			require.Equal(t, tc.want, proxy.CurrentRuntimeSettings().BPSRoundConvergenceLimit)
		})
	}
}
