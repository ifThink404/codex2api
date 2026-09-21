package auth

import (
	"context"
	"time"

	"github.com/codex2api/cache"
	"github.com/google/uuid"
)

// RestoreLegacyAPISession moves a relay's old window to the canonical root
// without consuming another slot or overwriting a root that has already won.
func (s *Store) RestoreLegacyAPISession(legacy, root string, account *Account) (int64, bool) {
	if s == nil || account == nil || !account.IsOpenAIResponsesAPI() || legacy == "" || root == "" {
		return 0, false
	}
	now := time.Now()
	if owner, found := s.LiveSessionAccountID(root, now); found {
		return owner, true
	}
	s.sessionMu.RLock()
	binding, found := s.sessionBindings[legacy]
	s.sessionMu.RUnlock()
	if !found {
		binding, found = s.getCachedSessionAffinity(legacy)
	}
	if !found || binding.accountID != account.ID() || !binding.expiresAt.After(now) {
		return 0, false
	}
	limits := account.SessionCapacityLimits()
	if limits.Enabled {
		s.ensureAccountSessionsLoaded(account, now)
	}
	s.sessionMu.Lock()
	if current, exists := s.sessionBindings[root]; exists && current.expiresAt.After(now) {
		s.sessionMu.Unlock()
		return current.accountID, true
	}
	if limits.Enabled {
		s.accountSessionMu.Lock()
		if s.accountSessions == nil {
			s.accountSessions = make(map[int64]map[string]*accountSessionState)
		}
		reservedCount := s.purgeExpiredAccountSessionsLocked(account.ID(), limits.IdleTTL, now)
		windows := s.accountSessions[account.ID()]
		if windows == nil {
			windows = make(map[string]*accountSessionState)
			s.accountSessions[account.ID()] = windows
		}
		if windows[root] == nil {
			state := windows[legacy]
			if state == nil {
				reserved, allowed := accountSessionSlotAvailable(int64(len(windows)), reservedCount, limits, false)
				if !allowed {
					s.accountSessionMu.Unlock()
					s.sessionMu.Unlock()
					return 0, false
				}
				state = &accountSessionState{lastSeen: now, usageStartedAt: now, usagePeriodID: uuid.NewString(), reserved: reserved}
			}
			state.sessionID = root
			windows[root] = state
		}
		delete(windows, legacy)
		s.accountSessionMu.Unlock()
	}
	if s.sessionBindings == nil {
		s.sessionBindings = make(map[string]sessionAffinity)
	}
	s.sessionBindings[root] = binding
	s.sessionMu.Unlock()
	if limits.Enabled {
		s.persistAccountSessions(account.ID(), now, legacy, root)
	}
	if s.tokenCache != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		_ = s.tokenCache.SetSessionAffinity(ctx, root, cache.SessionAffinityBinding{AccountID: binding.accountID, ProxyURL: binding.proxyURL}, time.Until(binding.expiresAt))
	}
	s.notifyRootAccountWaiters(root)
	return binding.accountID, true
}
