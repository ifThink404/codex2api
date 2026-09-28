package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
)

func codexRefreshRecoveryFixture(t *testing.T, provider http.HandlerFunc) (*Store, *database.DB, cache.TokenCache, *Account) {
	t.Helper()
	server := httptest.NewServer(provider)
	t.Cleanup(server.Close)
	previous := ResinRequestDecorator
	ResinRequestDecorator = func(string, string) string { return server.URL }
	t.Cleanup(func() { ResinRequestDecorator = previous })
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "recovery.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	id, err := db.InsertAccountWithCredentials(t.Context(), "recovery", map[string]interface{}{
		"access_token": "old-at", "refresh_token": "old-rt", "plan_type": "plus",
		"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
	}, "")
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	tc := cache.NewMemory(16)
	t.Cleanup(func() { _ = tc.Close() })
	s := NewStore(db, tc, &database.SystemSettings{MaxConcurrency: 2, FastSchedulerEnabled: true})
	t.Cleanup(s.Stop)
	if err := s.LoadAccountByID(t.Context(), id); err != nil {
		t.Fatalf("LoadAccountByID: %v", err)
	}
	a := s.FindByID(id)
	if a == nil {
		t.Fatal("account not loaded")
	}
	return s, db, tc, a
}

func sendCodexRefreshSuccess(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "new-at", "refresh_token": "new-rt", "expires_in": 3600})
}

func TestCodexRefreshSuccessRecoversAuthorization(t *testing.T) {
	s, db, tc, a := codexRefreshRecoveryFixture(t, func(w http.ResponseWriter, r *http.Request) { sendCodexRefreshSuccess(w) })
	s.MarkCooldownWithError(a, time.Hour, "unauthorized", "access token invalidated")
	if err := s.RefreshSingle(t.Context(), a.DBID); err != nil {
		t.Fatalf("RefreshSingle: %v", err)
	}
	if got := a.RuntimeStatus(); got != "active" {
		t.Fatalf("runtime status = %q, want active", got)
	}
	a.mu.RLock()
	tier, lastUnauthorized := a.HealthTier, a.LastUnauthorizedAt
	a.mu.RUnlock()
	if tier == HealthTierBanned || !lastUnauthorized.IsZero() {
		t.Fatalf("authorization failure kept: tier=%v lastUnauthorized=%v", tier, lastUnauthorized)
	}
	if _, present, err := tc.GetRuntime(t.Context(), accountCooldownCacheNamespace, accountCooldownRuntimeKey(a.DBID)); err != nil || present {
		t.Fatalf("stale 401 cache must not block the next dispatch: present=%v err=%v", present, err)
	}
	row, err := db.GetAccountByID(t.Context(), a.DBID)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if row.CooldownReason != "" || row.ErrorMessage != "" || row.Status != "active" {
		t.Fatalf("persisted state = status %q reason %q error %q", row.Status, row.CooldownReason, row.ErrorMessage)
	}
	if !a.IsAvailable() {
		t.Fatal("recovered account must be selectable")
	}
	second := NewStore(db, tc, &database.SystemSettings{MaxConcurrency: 2})
	t.Cleanup(second.Stop)
	if err := second.LoadAccountByID(t.Context(), a.DBID); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := second.FindByID(a.DBID).RuntimeStatus(); got != "active" {
		t.Fatalf("reload restored authorization failure: %q", got)
	}
}

func TestCodexRefreshSuccessPreservesQuotaCooldown(t *testing.T) {
	for _, duringExchange := range []bool{false, true} {
		name := map[bool]string{false: "existing", true: "arrives_during_refresh"}[duringExchange]
		t.Run(name, func(t *testing.T) {
			var s *Store
			var a *Account
			s, db, _, a := codexRefreshRecoveryFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if duringExchange {
					s.MarkCooldownWithError(a, time.Hour, "rate_limited", "quota is still exhausted")
				}
				sendCodexRefreshSuccess(w)
			})
			_ = s
			s.MarkCooldownWithError(a, time.Hour, "unauthorized", "access token invalidated")
			if !duringExchange {
				s.MarkCooldownWithError(a, time.Hour, "rate_limited", "quota is still exhausted")
			}
			if err := s.RefreshSingle(t.Context(), a.DBID); err != nil {
				t.Fatalf("RefreshSingle: %v", err)
			}
			if got := a.GetCooldownReason(); got != "rate_limited" || a.IsAvailable() {
				t.Fatalf("quota cooldown lost: reason=%q available=%v", got, a.IsAvailable())
			}
			row, err := db.GetAccountByID(t.Context(), a.DBID)
			if err != nil {
				t.Fatalf("GetAccountByID: %v", err)
			}
			if row.CooldownReason != "rate_limited" {
				t.Fatalf("persisted cooldown reason = %q, want rate_limited", row.CooldownReason)
			}
			if cached, ok := s.getCachedAccountCooldown(a.DBID); !ok || cached.Reason != "rate_limited" {
				t.Fatalf("cached cooldown = %+v ok=%v", cached, ok)
			}
		})
	}
}

func TestCodexRefreshFailureDoesNotRecoverAuthorization(t *testing.T) {
	s, _, _, a := codexRefreshRecoveryFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_grant"}}`))
	})
	s.MarkCooldownWithError(a, time.Hour, "unauthorized", "access token invalidated")
	if err := s.RefreshSingle(t.Context(), a.DBID); err == nil {
		t.Fatal("refresh failure must be reported")
	}
	if a.IsAvailable() {
		t.Fatal("failed refresh must not recover authorization")
	}
}

func TestCodexRefreshDoesNotTrustReloadedRejectedAccessToken(t *testing.T) {
	var exchanges atomic.Int64
	s, db, _, a := codexRefreshRecoveryFixture(t, func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		sendCodexRefreshSuccess(w)
	})
	// Another writer rotated only the RT; the stored AT is the one that got 401.
	if err := db.UpdateCredentials(t.Context(), a.DBID, map[string]interface{}{"refresh_token": "rotated-rt"}); err != nil {
		t.Fatalf("UpdateCredentials: %v", err)
	}
	s.MarkCooldownWithError(a, time.Hour, "unauthorized", "access token invalidated")
	if err := s.RefreshSingle(t.Context(), a.DBID); err != nil {
		t.Fatalf("RefreshSingle: %v", err)
	}
	if got := exchanges.Load(); got != 1 {
		t.Fatalf("OAuth exchanges = %d, want 1 (a rotated RT alone must not recover the rejected AT)", got)
	}
	if a.GetAccessToken() != "new-at" || !a.IsAvailable() {
		t.Fatalf("account not recovered with a new AT: at=%q available=%v", a.GetAccessToken(), a.IsAvailable())
	}
}
