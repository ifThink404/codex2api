package admin

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
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
