package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestSessionAutoLockAndVaultSettingsRoundtrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := newSettingsTestHandler(t)
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings()) })
	get := func() map[string]any {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil)
		h.GetSettings(c)
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode (%d): %v", rec.Code, err)
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
	if initial["codex_session_auto_lock_enabled"] != false || initial["codex_session_auto_lock_threshold"] != float64(3) || initial["codex_turn_state_vault_enabled"] != true {
		t.Fatalf("defaults: %v", initial)
	}
	if code := put(`{"codex_session_auto_lock_enabled":true,"codex_session_auto_lock_threshold":5,"codex_turn_state_vault_enabled":false}`); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	after := get()
	if after["codex_session_auto_lock_enabled"] != true || after["codex_session_auto_lock_threshold"] != float64(5) || after["codex_turn_state_vault_enabled"] != false {
		t.Fatalf("persisted: %v", after)
	}
	rt := proxy.CurrentRuntimeSettings()
	if !rt.CodexSessionAutoLockEnabled || rt.CodexSessionAutoLockThreshold != 5 || rt.CodexTurnStateVaultEnabled {
		t.Fatalf("runtime not hot-applied: %#v", rt)
	}
	if code := put(`{"codex_session_auto_lock_threshold":0}`); code != http.StatusOK {
		t.Fatalf("PUT threshold 0 = %d", code)
	}
	if get()["codex_session_auto_lock_threshold"] != float64(3) {
		t.Fatal("threshold 0 must normalize to 3")
	}
}
