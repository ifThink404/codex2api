package auth

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/database"
)

func TestSessionBalanceSelectionAcrossEngines(t *testing.T) {
	for _, engine := range []string{"legacy", "shadow", "indexed"} {
		for _, mode := range []string{database.SessionBalanceWindow, database.SessionBalanceSession} {
			t.Run(engine+"/"+mode, func(t *testing.T) {
				a := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 8)
				b := newFastSchedulerTestAccount(2, HealthTierHealthy, 100, 8)
				c := newFastSchedulerTestAccount(3, HealthTierHealthy, 100, 8)
				store := newSessionWindowBalanceStore(t, a, b, c)
				t.Cleanup(store.Stop)
				store.SetSchedulerEngine(engine)
				store.SetSessionBalanceMode(mode)
				for i := 0; i < 3; i++ {
					admitWindow(t, store, a, fmt.Sprintf("busy-session-%d", i))
				}
				admitWindow(t, store, c, "one-session")
				// The lowest active count alone is misleading: a has one buffered
				// slot, b has five, and c has no spare reservation.
				atomic.StoreInt64(&a.ActiveRequests, 1)
				atomic.StoreInt64(&a.OccupiedRequests, 2)
				atomic.StoreInt64(&b.ActiveRequests, 1)
				atomic.StoreInt64(&b.OccupiedRequests, 6)
				atomic.StoreInt64(&c.ActiveRequests, 2)
				atomic.StoreInt64(&c.OccupiedRequests, 2)
				got, _ := store.NextForSession("new-session", 0, nil)
				if got == nil {
					t.Fatal("no account selected")
				}
				defer store.Release(got)
				want := a.DBID
				if mode == database.SessionBalanceSession {
					want = b.DBID
				}
				if got.DBID != want {
					t.Fatalf("selected %d, want %d", got.DBID, want)
				}
				store.BindSessionAffinity("new-session", got, "")
				// Once bound, raising its load must not relocate the conversation.
				atomic.StoreInt64(&got.ActiveRequests, 6)
				atomic.StoreInt64(&got.OccupiedRequests, 6)
				reused, _ := store.NextForSession("new-session", 0, nil)
				if reused == nil || reused.DBID != want {
					t.Fatal("balancing moved an existing conversation")
				}
				store.Release(reused)
			})
		}
	}
}

func TestSessionBalanceConcurrentFreshAdmission(t *testing.T) {
	for _, engine := range []string{"legacy", "shadow", "indexed"} {
		for _, mode := range []string{database.SessionBalanceWindow, database.SessionBalanceSession} {
			t.Run(engine+"/"+mode, func(t *testing.T) {
				accounts := []*Account{
					newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 8),
					newFastSchedulerTestAccount(2, HealthTierHealthy, 100, 8),
					newFastSchedulerTestAccount(3, HealthTierHealthy, 100, 8),
				}
				store := newSessionWindowBalanceStore(t, accounts...)
				t.Cleanup(store.Stop)
				store.SetSchedulerEngine(engine)
				store.SetSessionBalanceMode(mode)
				start := make(chan struct{})
				results := make(chan *Account, 12)
				var wg sync.WaitGroup
				for i := 0; i < 12; i++ {
					wg.Add(1)
					go func(i int) {
						defer wg.Done()
						<-start
						account, _ := store.NextForSession(fmt.Sprintf("concurrent-%d", i), 0, nil)
						results <- account
					}(i)
				}
				close(start)
				wg.Wait()
				close(results)
				counts := map[int64]int{}
				for account := range results {
					if account == nil {
						t.Error("concurrent request was rejected")
						continue
					}
					counts[account.DBID]++
					store.Release(account)
				}
				for _, account := range accounts {
					if counts[account.DBID] != 4 {
						t.Fatalf("uneven concurrent assignment: %v", counts)
					}
				}
			})
		}
	}
}

func TestSessionBalanceEligibilityAndRootlessRequests(t *testing.T) {
	for _, engine := range []string{"legacy", "shadow", "indexed"} {
		for _, constraint := range []string{"priority", "health", "excluded", "filtered", "disabled", "full"} {
			t.Run(engine+"/"+constraint, func(t *testing.T) {
				wanted := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 8)
				other := newFastSchedulerTestAccount(2, HealthTierHealthy, 100, 8)
				if constraint == "priority" {
					wanted.SetSchedulerPriority(10)
				}
				if constraint == "health" {
					other.LastTimeoutAt = time.Now()
				}
				store := newSessionWindowBalanceStore(t, wanted, other)
				t.Cleanup(store.Stop)
				store.SetSchedulerEngine(engine)
				store.SetSessionBalanceMode(database.SessionBalanceWindow)
				atomic.StoreInt64(&wanted.ActiveRequests, 3)
				atomic.StoreInt64(&wanted.OccupiedRequests, 3)
				var exclude map[int64]bool
				var filter AccountFilter
				switch constraint {
				case "excluded":
					exclude = map[int64]bool{other.DBID: true}
				case "filtered":
					filter = func(a *Account) bool { return a.DBID != other.DBID }
				case "disabled":
					atomic.StoreInt32(&other.Disabled, 1)
				case "full":
					atomic.StoreInt64(&other.OccupiedRequests, 8)
				}
				got, _ := store.NextForSessionWithFilter("", 0, exclude, filter)
				if got == nil || got.DBID != wanted.DBID {
					t.Fatalf("rootless selection did not respect %s", constraint)
				}
				store.Release(got)
				if counts := store.accountWindowCountsForScheduling(store.Accounts(), time.Now()); counts[wanted.DBID] != 0 {
					t.Fatalf("invented a session record for a rootless request: %v", counts)
				}
			})
		}
	}
}

func TestSessionBalanceWithoutSessionCapacity(t *testing.T) {
	for _, engine := range []string{"legacy", "shadow", "indexed"} {
		t.Run(engine, func(t *testing.T) {
			store := NewStore(nil, nil, &database.SystemSettings{
				MaxConcurrency: 8, AffinityMode: "strict", SessionBalanceMode: database.SessionBalanceSession,
			})
			t.Cleanup(store.Stop)
			busy := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 8)
			idle := newFastSchedulerTestAccount(2, HealthTierHealthy, 100, 8)
			store.AddAccount(busy)
			store.AddAccount(idle)
			store.SetSchedulerEngine(engine)
			store.BindSessionAffinity("existing-a", busy, "")
			store.BindSessionAffinity("existing-b", busy, "")
			got, _ := store.NextForSession("fresh", 0, nil)
			if got == nil || got.DBID != idle.DBID {
				t.Fatal("session balancing ignored ordinary affinity bindings")
			}
			store.Release(got)
			reused, _ := store.NextForSession("existing-a", 0, nil)
			if reused == nil || reused.DBID != busy.DBID {
				t.Fatal("balancing moved an existing strict binding")
			}
			store.Release(reused)
		})
	}
}
