package auth

import (
	"sync/atomic"

	"github.com/codex2api/database"
)

func (s *Store) GetSessionBalanceMode() string {
	if s != nil {
		if mode, ok := s.sessionBalanceMode.Load().(string); ok {
			return database.NormalizeSessionBalanceMode(mode, false)
		}
	}
	return database.SessionBalanceDefault
}

func (s *Store) SetSessionBalanceMode(mode string) {
	if s != nil {
		s.sessionBalanceMode.Store(database.NormalizeSessionBalanceMode(mode, false))
	}
}

type freshSessionBalance struct {
	mode         string
	sessionCount map[int64]int64
}

type sessionBalanceLoad struct {
	occupied int64
	active   int64
	sessions int64
}

func (b *freshSessionBalance) load(account *Account) sessionBalanceLoad {
	return sessionBalanceLoad{
		occupied: accountOccupiedRequests(account),
		active:   atomic.LoadInt64(&account.ActiveRequests),
		sessions: b.sessionCount[account.DBID],
	}
}

// compare keeps the balancing policy identical across indexed and legacy
// selection. Health tiers and operator priorities are compared by the caller.
func (b *freshSessionBalance) compare(a, c sessionBalanceLoad) int {
	left := [3]int64{a.occupied, a.active, a.sessions}
	right := [3]int64{c.occupied, c.active, c.sessions}
	if b.mode == database.SessionBalanceSession {
		left = [3]int64{a.sessions, a.occupied, a.active}
		right = [3]int64{c.sessions, c.occupied, c.active}
	}
	for i := range left {
		if left[i] < right[i] {
			return -1
		}
		if left[i] > right[i] {
			return 1
		}
	}
	return 0
}
