package admin

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestRuntimeStatusIncludesSessionGuards(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	h := &Handler{store: store}
	status := h.buildRuntimeStatus(context.Background(), httptest.NewRequest("GET", "/api/admin/runtime", nil))
	guards := status.SessionGuards
	if guards.StartedAt == "" {
		t.Fatal("session_guards.started_at missing")
	}
	if guards.Settings.NoBorrowHoldSeconds != 20 || guards.Settings.InitialSessionMaxAgeSeconds != 180 {
		t.Fatalf("settings snapshot = %+v", guards.Settings)
	}
	if guards.TurnState.Accounts == nil || guards.Borrow.Borrowed != 0 || guards.InitialSession.SinceStart.Samples != 0 {
		t.Fatalf("fresh counters = %+v", guards)
	}
}

// TestRuntimeStatusAutoLockReportsLiveEnabledFlag 固化线上那次矛盾读数：设置里
// 自动锁定是开的、锁也在产生，运行状态却报 auto_lock.enabled=false。快照必须在
// 每条分支（有 / 没有 auth cache proxy）都反映 CurrentRuntimeSettings() 的当前值。
func TestRuntimeStatusAutoLockReportsLiveEnabledFlag(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	proxy.ApplyRuntimeSettingsFromSystem(&database.SystemSettings{
		CodexSessionAutoLockEnabled:   true,
		CodexSessionAutoLockThreshold: 5,
	})
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})

	assertLiveAutoLock := func(t *testing.T, h *Handler) {
		t.Helper()
		snapshot := h.buildRuntimeStatus(context.Background(), httptest.NewRequest("GET", "/api/admin/runtime-status", nil)).SessionGuards.AutoLock
		if !snapshot.Enabled || snapshot.Threshold != 5 {
			t.Fatalf("snapshot auto_lock = %+v, want enabled with threshold 5", snapshot)
		}
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest("GET", "/api/admin/runtime-status", nil)
		h.GetRuntimeStatus(c)
		var payload struct {
			SessionGuards struct {
				AutoLock struct {
					Enabled   bool `json:"enabled"`
					Threshold int  `json:"threshold"`
				} `json:"auto_lock"`
			} `json:"session_guards"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode runtime status JSON: %v (body %s)", err, recorder.Body.String())
		}
		if !payload.SessionGuards.AutoLock.Enabled || payload.SessionGuards.AutoLock.Threshold != 5 {
			t.Fatalf("session_guards.auto_lock JSON = %+v, want enabled with threshold 5", payload.SessionGuards.AutoLock)
		}
	}

	t.Run("without auth cache proxy", func(t *testing.T) {
		assertLiveAutoLock(t, &Handler{store: store})
	})
	t.Run("with auth cache proxy", func(t *testing.T) {
		db := newTestAdminDB(t)
		p := proxy.NewHandler(store, db, &config.Config{}, nil)
		t.Cleanup(p.CloseAPIKeyAuthCache)
		h := &Handler{store: store}
		h.SetAPIKeyAuthCacheHandler(p)
		assertLiveAutoLock(t, h)
	})
}
