package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
)

func codexRefreshRecoveryFixture(t *testing.T, provider http.HandlerFunc) (*Store, *database.DB, cache.TokenCache, *Account) {
	t.Helper()
	server := httptest.NewServer(provider)
	t.Cleanup(server.Close)
	previous := ResinRequestDecorator
	ResinRequestDecorator = func(string, string) string { return server.URL }
	t.Cleanup(func() { ResinRequestDecorator = previous })
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "recovery.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	id, err := db.InsertAccountWithCredentials(t.Context(), "recovery", map[string]interface{}{
		"access_token": "old-at", "refresh_token": "old-rt", "plan_type": "plus",
		"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
	}, "")
	require.NoError(t, err)
	tc := cache.NewMemory(16)
	t.Cleanup(func() { _ = tc.Close() })
	s := NewStore(db, tc, &database.SystemSettings{MaxConcurrency: 2, FastSchedulerEnabled: true})
	t.Cleanup(s.Stop)
	require.NoError(t, s.LoadAccountByID(t.Context(), id))
	a := s.FindByID(id)
	require.NotNil(t, a)
	return s, db, tc, a
}

func sendCodexRefreshSuccess(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "new-at", "refresh_token": "new-rt", "expires_in": 3600})
}

func markCodexRefreshUnauthorized(s *Store, a *Account) {
	atomic.StoreInt32(&a.Disabled, 1)
	s.MarkCooldownWithError(a, time.Hour, "unauthorized", "access token invalidated")
	a.mu.Lock()
	a.PermanentRefreshFailures = 2
	a.mu.Unlock()
}

func TestCodexRefreshSuccessRecoversAuthorization(t *testing.T) {
	for _, state := range []string{"unauthorized", "terminal_refresh_error", "manual_pause"} {
		t.Run(state, func(t *testing.T) {
			s, db, tc, a := codexRefreshRecoveryFixture(t, func(w http.ResponseWriter, r *http.Request) { sendCodexRefreshSuccess(w) })
			markCodexRefreshUnauthorized(s, a)
			if state == "terminal_refresh_error" {
				s.MarkError(a, "refresh token invalidated")
			}
			if state == "manual_pause" {
				require.NoError(t, db.SetAccountEnabled(t.Context(), a.DBID, false))
				atomic.StoreInt32(&a.DispatchPaused, 1)
			}
			require.NoError(t, s.RefreshSingle(t.Context(), a.DBID))
			require.Equal(t, "active", a.RuntimeStatus())
			require.Zero(t, atomic.LoadInt32(&a.Disabled))
			a.mu.RLock()
			tier, failures, message := a.HealthTier, a.PermanentRefreshFailures, a.ErrorMsg
			a.mu.RUnlock()
			require.NotEqual(t, HealthTierBanned, tier)
			require.Zero(t, failures)
			require.Empty(t, message)
			_, present, err := tc.GetRuntime(t.Context(), accountCooldownCacheNamespace, accountCooldownRuntimeKey(a.DBID))
			require.NoError(t, err)
			require.False(t, present, "stale 401 cache must not block the next dispatch")
			row, err := db.GetAccountByID(t.Context(), a.DBID)
			require.NoError(t, err)
			require.Empty(t, row.CooldownReason)
			require.Empty(t, row.ErrorMessage)
			require.Equal(t, "active", row.Status)
			require.Equal(t, state != "manual_pause", row.Enabled)
			require.Equal(t, state != "manual_pause", a.IsAvailable())
			if state != "manual_pause" {
				picked := s.Next()
				require.Same(t, a, picked, "must be selectable, not just display active")
				s.Release(picked)
			}
			second := NewStore(db, tc, &database.SystemSettings{MaxConcurrency: 2})
			t.Cleanup(second.Stop)
			require.NoError(t, second.LoadAccountByID(t.Context(), a.DBID))
			require.Equal(t, "active", second.FindByID(a.DBID).RuntimeStatus(), "reload must not restore authorization failure")
		})
	}
}

func TestCodexRefreshSuccessPreservesQuotaCooldown(t *testing.T) {
	for _, duringExchange := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing", true: "arrives_during_refresh"}[duringExchange], func(t *testing.T) {
			var s *Store
			var a *Account
			var db *database.DB
			s, db, _, a = codexRefreshRecoveryFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if duringExchange {
					s.MarkCooldownWithError(a, time.Hour, "rate_limited", "quota is still exhausted")
				}
				sendCodexRefreshSuccess(w)
			})
			markCodexRefreshUnauthorized(s, a)
			if !duringExchange {
				s.MarkCooldownWithError(a, time.Hour, "rate_limited", "quota is still exhausted")
			}
			require.NoError(t, s.RefreshSingle(t.Context(), a.DBID))
			require.Equal(t, "rate_limited", a.GetCooldownReason())
			require.NotEqual(t, "unauthorized", a.RuntimeStatus())
			require.False(t, a.IsAvailable())
			row, err := db.GetAccountByID(t.Context(), a.DBID)
			require.NoError(t, err)
			require.Equal(t, "rate_limited", row.CooldownReason)
			require.Equal(t, "quota is still exhausted", row.ErrorMessage)
			cached, ok := s.getCachedAccountCooldown(a.DBID)
			require.True(t, ok)
			require.Equal(t, "rate_limited", cached.Reason)
		})
	}
}

func TestCodexRefreshFailureDoesNotRecoverAuthorization(t *testing.T) {
	s, _, _, a := codexRefreshRecoveryFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_grant"}}`))
	})
	markCodexRefreshUnauthorized(s, a)
	require.Error(t, s.RefreshSingle(t.Context(), a.DBID))
	require.False(t, a.IsAvailable())
	require.NotEqual(t, "active", a.RuntimeStatus())
}

func TestCodexRefreshCachedOldTokenCannotRecoverAuthorization(t *testing.T) {
	var exchanges atomic.Int32
	s, _, tc, a := codexRefreshRecoveryFixture(t, func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		sendCodexRefreshSuccess(w)
	})
	markCodexRefreshUnauthorized(s, a)
	require.NoError(t, tc.SetAccessToken(context.Background(), a.DBID, "old-at", time.Hour))
	require.NoError(t, s.refreshAccount(t.Context(), a))
	require.EqualValues(t, 1, exchanges.Load(), "an invalidated token cache hit is not a successful refresh")
	require.True(t, a.IsAvailable())
}

func TestCodexRefreshClearsOrphanedAuthorizationCache(t *testing.T) {
	s, _, tc, a := codexRefreshRecoveryFixture(t, func(w http.ResponseWriter, r *http.Request) { sendCodexRefreshSuccess(w) })
	payload, err := json.Marshal(runtimeCooldownRecord{
		Reason: "unauthorized", ResetAt: time.Now().Add(time.Hour), UpdatedAt: time.Now().Add(-time.Minute),
	})
	require.NoError(t, err)
	require.NoError(t, tc.SetRuntime(t.Context(), accountCooldownCacheNamespace, accountCooldownRuntimeKey(a.DBID), payload, time.Hour))
	require.NoError(t, s.RefreshSingle(t.Context(), a.DBID))
	_, present := s.getCachedAccountCooldown(a.DBID)
	require.False(t, present)
	picked := s.Next()
	require.Same(t, a, picked)
	s.Release(picked)
}

func TestCodexRefreshReusesCompletedRefresh(t *testing.T) {
	for _, source := range []string{"cache", "database", "rt_only"} {
		t.Run(source, func(t *testing.T) {
			var exchanges atomic.Int32
			s, db, tc, a := codexRefreshRecoveryFixture(t, func(w http.ResponseWriter, r *http.Request) {
				exchanges.Add(1)
				sendCodexRefreshSuccess(w)
			})
			markCodexRefreshUnauthorized(s, a)
			if source == "cache" {
				require.NoError(t, tc.SetAccessToken(t.Context(), a.DBID, "new-at", time.Hour))
			} else {
				credentials := map[string]interface{}{"refresh_token": "rotated-rt"}
				if source == "database" {
					credentials["access_token"] = "new-at"
				}
				require.NoError(t, db.UpdateCredentials(t.Context(), a.DBID, credentials))
			}
			require.NoError(t, s.refreshAccount(t.Context(), a))
			if source == "rt_only" {
				require.EqualValues(t, 1, exchanges.Load(), "changing only RT does not validate the rejected AT")
			} else {
				require.Zero(t, exchanges.Load())
			}
			require.True(t, a.IsAvailable())
			require.Equal(t, "active", a.RuntimeStatus())
			row, err := db.GetAccountByID(t.Context(), a.DBID)
			require.NoError(t, err)
			require.Empty(t, row.CooldownReason)
			require.Empty(t, row.ErrorMessage)
			_, cached := s.getCachedAccountCooldown(a.DBID)
			require.False(t, cached)
		})
	}
}

func TestCodexRefreshRecoversSharedCredentialRoutes(t *testing.T) {
	s, db, _, source := codexRefreshRecoveryFixture(t, func(w http.ResponseWriter, r *http.Request) { sendCodexRefreshSuccess(w) })
	for _, reason := range []string{"unauthorized", "rate_limited"} {
		id, err := db.InsertAccountWithCredentials(t.Context(), reason, map[string]interface{}{
			"access_token": "old-at", "refresh_token": "old-rt", "plan_type": "plus",
			"expires_at":     time.Now().Add(time.Hour).Format(time.RFC3339),
			"custom_headers": map[string]string{"Chatgpt-Account-Id": reason + "-workspace"},
		}, "")
		require.NoError(t, err)
		require.NoError(t, s.LoadAccountByID(t.Context(), id))
		a := s.FindByID(id)
		markCodexRefreshUnauthorized(s, a)
		if reason == "rate_limited" {
			s.MarkCooldownWithError(a, time.Hour, reason, "keep quota")
		}
	}
	markCodexRefreshUnauthorized(s, source)
	require.NoError(t, s.RefreshSingle(t.Context(), source.DBID))
	for _, a := range s.accountSnapshotAccounts() {
		row, err := db.GetAccountByID(t.Context(), a.DBID)
		require.NoError(t, err)
		require.Equal(t, "new-at", row.GetCredential("access_token"))
		require.Equal(t, "new-rt", row.GetCredential("refresh_token"))
		require.Zero(t, atomic.LoadInt32(&a.Disabled))
		if a.DBID == source.DBID {
			continue
		}
		a.mu.RLock()
		route := a.CustomHeaders["Chatgpt-Account-Id"]
		a.mu.RUnlock()
		require.Equal(t, row.GetCredentialStringMap("custom_headers")["Chatgpt-Account-Id"], route)
		cached, present := s.getCachedAccountCooldown(a.DBID)
		if route == "rate_limited-workspace" {
			require.False(t, a.IsAvailable())
			require.Equal(t, "rate_limited", row.CooldownReason)
			require.Equal(t, "keep quota", row.ErrorMessage)
			require.True(t, present)
			require.Equal(t, "rate_limited", cached.Reason)
		} else {
			require.Equal(t, "unauthorized-workspace", route)
			require.True(t, a.IsAvailable())
			require.Empty(t, row.CooldownReason)
			require.False(t, present)
		}
	}
}

// Insert a newer failure between the cache read and conditional deletion.
type codexRecoveryRaceCache struct {
	cache.TokenCache
	cache.RuntimeOwnerStore
	afterRead func()
}

func (c *codexRecoveryRaceCache) GetRuntime(ctx context.Context, namespace, key string) (json.RawMessage, bool, error) {
	payload, found, err := c.TokenCache.GetRuntime(ctx, namespace, key)
	if namespace == accountCooldownCacheNamespace && c.afterRead != nil {
		fn := c.afterRead
		c.afterRead = nil
		fn()
	}
	return payload, found, err
}

func TestCodexRefreshCleanupPreservesNewerFailure(t *testing.T) {
	for _, phase := range []string{"before_persistence", "during_cache_cleanup"} {
		for _, reason := range []string{"rate_limited", "unauthorized"} {
			t.Run(phase+"/"+reason, func(t *testing.T) {
				s, db, tc, a := codexRefreshRecoveryFixture(t, func(w http.ResponseWriter, r *http.Request) { sendCodexRefreshSuccess(w) })
				markCodexRefreshUnauthorized(s, a)
				a.mu.Lock()
				recovery := a.recoverCodexRefreshLocked(time.Now())
				a.mu.Unlock()
				newFailure := func() { s.MarkCooldownWithError(a, 2*time.Hour, reason, "new failure") }
				if phase == "before_persistence" {
					newFailure()
				} else {
					s.tokenCache = &codexRecoveryRaceCache{TokenCache: tc, RuntimeOwnerStore: tc.(cache.RuntimeOwnerStore), afterRead: newFailure}
				}
				require.NoError(t, s.persistCodexRefreshRecovery(t.Context(), a, recovery))
				require.Equal(t, reason, a.GetCooldownReason())
				row, err := db.GetAccountByID(t.Context(), a.DBID)
				require.NoError(t, err)
				require.Equal(t, reason, row.CooldownReason)
				require.Equal(t, "new failure", row.ErrorMessage)
				cached, present := s.getCachedAccountCooldown(a.DBID)
				require.True(t, present)
				require.Equal(t, reason, cached.Reason)
			})
		}
	}
}

func TestCodexRefreshPreservesUsageAndModelCooldown(t *testing.T) {
	s, db, tc, a := codexRefreshRecoveryFixture(t, func(w http.ResponseWriter, r *http.Request) { sendCodexRefreshSuccess(w) })
	markCodexRefreshUnauthorized(s, a)
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	a.mu.Lock()
	a.UsagePercent5h, a.UsagePercent5hValid, a.Reset5hAt = 100, true, reset
	a.mu.Unlock()
	require.NoError(t, db.UpdateUsageSnapshot5h(t.Context(), a.DBID, 100, reset, time.Now()))
	s.MarkModelCooldown(a, "gpt-5.6-sol", time.Hour, "model_capacity")
	require.NoError(t, s.RefreshSingle(t.Context(), a.DBID))
	a.mu.RLock()
	percent, valid, resetAfter := a.UsagePercent5h, a.UsagePercent5hValid, a.Reset5hAt
	modelCooldown := a.ModelCooldowns["gpt-5.6-sol"]
	a.mu.RUnlock()
	require.EqualValues(t, 100, percent)
	require.True(t, valid)
	require.Equal(t, reset, resetAfter)
	require.Equal(t, "model_capacity", modelCooldown.Reason)
	_, cached, err := tc.GetRuntime(t.Context(), modelCooldownCacheNamespace, modelCooldownRuntimeKey(a.DBID, "gpt-5.6-sol"))
	require.NoError(t, err)
	require.True(t, cached)
	row, err := db.GetAccountByID(t.Context(), a.DBID)
	require.NoError(t, err)
	require.Equal(t, reset.Format(time.RFC3339), row.GetCredential("codex_5h_reset_at"))
	require.False(t, a.IsAvailable(), "successful OAuth must not erase exhausted usage")
}
