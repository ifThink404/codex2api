package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/codex2api/cache"
)

type codexRefreshRecovery struct {
	reason     string
	until      time.Time
	message    string
	observedAt time.Time
	clear      bool
}

// Caller holds mu. Disabled is the immediate 401 fence; administrative pauses
// are represented separately by DispatchPaused and must survive refresh.
func (a *Account) codexAuthorizationRejectedLocked() bool {
	return atomic.LoadInt32(&a.Disabled) != 0 || a.HealthTier == HealthTierBanned ||
		a.CooldownReason == "unauthorized" || a.PermanentRefreshFailures > 0
}

// Only call after an OAuth exchange succeeds or a different, usable token is
// loaded from a completed refresh. A cache hit for the rejected AT is not proof.
// Use current state: an unrelated 429 may have arrived while OAuth was in flight.
func (a *Account) recoverCodexRefreshLocked(now time.Time) codexRefreshRecovery {
	state := codexRefreshRecovery{reason: a.CooldownReason, until: a.CooldownUtil, message: a.ErrorMsg, observedAt: now}
	authFailed := a.codexAuthorizationRejectedLocked()
	keepCooldown := a.Status == StatusCooldown && now.Before(a.CooldownUtil) && a.CooldownReason != "unauthorized"
	state.clear = !keepCooldown
	if !keepCooldown {
		a.Status = StatusReady
		a.ErrorMsg = ""
		a.CooldownReason = ""
		a.CooldownUtil = time.Time{}
	}
	atomic.StoreInt32(&a.Disabled, 0)
	a.PermanentRefreshFailures = 0
	if a.HealthTier == HealthTierBanned {
		a.HealthTier = HealthTierWarm
	}
	if authFailed {
		a.LastUnauthorizedAt = time.Time{}
		if !keepCooldown {
			a.FailureStreak = 0
			if a.LastFailureKind == "unauthorized" {
				a.LastFailureKind = ""
			}
		}
	}
	return state
}

func (s *Store) persistCodexRefreshRecovery(ctx context.Context, acc *Account, state codexRefreshRecovery) error {
	if !state.clear {
		return nil
	}
	if s.db != nil {
		cleared, err := s.db.ClearAccountErrorIfUnchanged(ctx, acc.DBID, state.reason, state.until, state.message)
		if err != nil {
			return fmt.Errorf("保存 OAuth 刷新后的账号恢复状态失败: %w", err)
		}
		if !cleared {
			// Another instance may have already recovered this row, or recorded a
			// newer failure. Only the former allows cleanup of the old cache.
			row, err := s.db.GetAccountByID(ctx, acc.DBID)
			if err != nil {
				return err
			}
			if row == nil || row.Status != "active" || row.CooldownReason != "" || row.ErrorMessage != "" {
				return nil
			}
		}
	}
	if s.tokenCache == nil {
		return nil
	}
	acc.mu.RLock()
	newFailure := acc.Status == StatusError || acc.CooldownReason != "" || acc.codexAuthorizationRejectedLocked()
	acc.mu.RUnlock()
	if newFailure {
		return nil
	}
	cacheCtx, cancel := cooldownRuntimeContext()
	defer cancel()
	key := accountCooldownRuntimeKey(acc.DBID)
	payload, found, err := s.tokenCache.GetRuntime(cacheCtx, accountCooldownCacheNamespace, key)
	if err != nil {
		return fmt.Errorf("读取待恢复的账号冷却缓存失败: %w", err)
	}
	if !found {
		return nil
	}
	var record runtimeCooldownRecord
	if json.Unmarshal(payload, &record) != nil {
		return nil
	}
	if record.UpdatedAt.After(state.observedAt) {
		return nil
	}
	matchesObserved := record.Reason == state.reason && record.ResetAt.Equal(state.until)
	// An earlier recovery on another instance can clear the database before
	// removing its cache record. A successful exchange can finish that cleanup.
	orphanedAuthorization := state.reason == "" && record.Reason == "unauthorized" && record.UpdatedAt.Before(state.observedAt)
	if !matchesObserved && !orphanedAuthorization {
		return nil
	}
	// Built-in memory and Redis caches both support atomic compare-and-delete;
	// never delete a newer 429/401 record that races with successful refresh.
	if owner, ok := s.tokenCache.(cache.RuntimeOwnerStore); ok {
		_, err = owner.CompareAndDeleteRuntimeOwner(cacheCtx, accountCooldownCacheNamespace, key, payload)
	} else {
		return fmt.Errorf("账号冷却缓存不支持条件删除，无法确认授权失败状态已恢复")
	}
	return err
}

func (s *Store) reuseCodexRefreshToken(ctx context.Context, acc *Account, token string, fallbackTTL time.Duration) (bool, error) {
	acc.mu.Lock()
	if token == "" || (token == acc.AccessToken && acc.codexAuthorizationRejectedLocked()) {
		acc.mu.Unlock()
		return false, nil
	}
	acc.AccessToken = token
	if acc.ExpiresAt.IsZero() || time.Until(acc.ExpiresAt) < 5*time.Minute {
		acc.ExpiresAt = time.Now().Add(fallbackTTL)
	}
	state := acc.recoverCodexRefreshLocked(time.Now())
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()
	err := s.persistCodexRefreshRecovery(ctx, acc, state)
	s.fastSchedulerUpdate(acc)
	return true, err
}

func (s *Store) finishReloadedCodexRefresh(ctx context.Context, acc *Account) error {
	acc.mu.Lock()
	state := acc.recoverCodexRefreshLocked(time.Now())
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	token, expiry := acc.AccessToken, acc.ExpiresAt
	acc.mu.Unlock()
	err := s.persistCodexRefreshRecovery(ctx, acc, state)
	s.fastSchedulerUpdate(acc)
	if s.tokenCache != nil && strings.TrimSpace(token) != "" {
		ttl := time.Until(expiry) - 5*time.Minute
		if expiry.IsZero() {
			ttl = 30 * time.Minute
		}
		if ttl > 0 {
			_ = s.tokenCache.SetAccessToken(ctx, acc.DBID, token, ttl)
		}
	}
	return err
}
