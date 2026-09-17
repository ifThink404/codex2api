package auth

import (
	"context"
	"testing"
	"time"
)

func TestNoBorrowHoldsInsteadOfSpilloverAtCapacity(t *testing.T) {
	bound := &Account{DBID: 1, AccessToken: "tok-1"}
	fallback := &Account{DBID: 2, AccessToken: "tok-2"}
	store := &Store{accounts: []*Account{bound, fallback}, maxConcurrency: 1}
	store.SetSessionNoBorrow(true, 5*time.Second)
	store.bindSessionAffinity("no-borrow", bound, "")

	held := store.TakePreferredAccountWithDispatch(bound.DBID, 0, nil, nil, DispatchPolicyStandard)
	if held != bound {
		t.Fatalf("held = %p, want bound %p", held, bound)
	}
	defer store.Release(held)

	selected, _, _ := store.NextForSessionWithDispatchGuard("no-borrow", 0, nil, nil, DispatchPolicyStandard)
	if selected != nil {
		store.Release(selected)
		t.Fatalf("no-borrow must not spill over, got account %d", selected.DBID)
	}
	if stats := store.SessionBorrowStats(); stats.Held != 1 || stats.Borrowed != 0 {
		t.Fatalf("stats = %+v, want held 1 borrowed 0", stats)
	}

	store.SetSessionNoBorrow(false, 5*time.Second)
	selected, _, guard := store.NextForSessionWithDispatchGuard("no-borrow", 0, nil, nil, DispatchPolicyStandard)
	if selected != fallback || !guard.PreservesExisting() {
		if selected != nil {
			store.Release(selected)
		}
		t.Fatalf("legacy spillover broken: selected=%v guard=%+v", selected, guard)
	}
	store.ReleaseForSessionWithGuard(selected, "no-borrow", guard)
	if stats := store.SessionBorrowStats(); stats.Held != 1 || stats.Borrowed != 1 {
		t.Fatalf("stats = %+v, want held 1 borrowed 1", stats)
	}
}

func TestNoBorrowUnboundSessionStillSelectsFreely(t *testing.T) {
	a := &Account{DBID: 1, AccessToken: "tok-1"}
	store := &Store{accounts: []*Account{a}, maxConcurrency: 1}
	store.SetSessionNoBorrow(true, 5*time.Second)
	selected, _, _ := store.NextForSessionWithDispatchGuard("fresh", 0, nil, nil, DispatchPolicyStandard)
	if selected != a {
		t.Fatalf("fresh session must still pick an account, got %v", selected)
	}
	store.Release(selected)
}

func TestNoBorrowWaitBorrowsAfterHold(t *testing.T) {
	bound := &Account{DBID: 1, AccessToken: "tok-1"}
	fallback := &Account{DBID: 2, AccessToken: "tok-2"}
	store := &Store{accounts: []*Account{bound, fallback}, maxConcurrency: 1}
	store.SetSessionNoBorrow(true, time.Second)
	store.sessionNoBorrowHoldNS.Store(int64(150 * time.Millisecond)) // 测试用短 hold，绕过 1s 下限
	store.bindSessionAffinity("no-borrow-wait", bound, "")
	held := store.TakePreferredAccountWithDispatch(bound.DBID, 0, nil, nil, DispatchPolicyStandard)
	if held != bound {
		t.Fatalf("held = %v", held)
	}
	defer store.Release(held)

	started := time.Now()
	selected, _, guard := store.WaitForSessionAvailableWithDispatchGuard(context.Background(), "no-borrow-wait", 2*time.Second, 0, nil, nil, DispatchPolicyStandard)
	elapsed := time.Since(started)
	if selected != fallback {
		if selected != nil {
			store.Release(selected)
		}
		t.Fatalf("after hold the wait must borrow the fallback, got %v", selected)
	}
	store.ReleaseForSessionWithGuard(selected, "no-borrow-wait", guard)
	if elapsed < 150*time.Millisecond {
		t.Fatalf("borrowed before the hold expired: %s", elapsed)
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("hold expiry was not honoured promptly: %s", elapsed)
	}
	if stats := store.SessionBorrowStats(); stats.Borrowed != 1 || stats.Held == 0 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestNoBorrowWaitReturnsBoundAccountWhenItFrees(t *testing.T) {
	bound := &Account{DBID: 1, AccessToken: "tok-1"}
	fallback := &Account{DBID: 2, AccessToken: "tok-2"}
	store := &Store{accounts: []*Account{bound, fallback}, maxConcurrency: 1}
	store.SetSessionNoBorrow(true, 10*time.Second)
	store.bindSessionAffinity("no-borrow-free", bound, "")
	held := store.TakePreferredAccountWithDispatch(bound.DBID, 0, nil, nil, DispatchPolicyStandard)
	if held != bound {
		t.Fatalf("held = %v", held)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		store.Release(held)
	}()
	selected, _, _ := store.WaitForSessionAvailableWithDispatchGuard(context.Background(), "no-borrow-free", 3*time.Second, 0, nil, nil, DispatchPolicyStandard)
	if selected != bound {
		if selected != nil {
			store.Release(selected)
		}
		t.Fatalf("wait must return the bound account once it frees, got %v", selected)
	}
	store.Release(selected)
	if stats := store.SessionBorrowStats(); stats.Borrowed != 0 {
		t.Fatalf("no borrow expected, stats = %+v", stats)
	}
}

// hold 到期重选失败后等待者必须仍是"单次入队"：否则同一等待者在 lane 里留下
// 第二个元素，离队时只摘掉一个，残留元素让后续 Release 在派发时永久卡死。
func TestNoBorrowHoldExpiryLeavesHubClean(t *testing.T) {
	bound := &Account{DBID: 1, AccessToken: "tok-1"}
	store := &Store{accounts: []*Account{bound}, maxConcurrency: 1} // 单账号：hold 到期后也无号可借
	store.SetSessionNoBorrow(true, time.Second)
	store.sessionNoBorrowHoldNS.Store(int64(100 * time.Millisecond))
	store.bindSessionAffinity("no-borrow-hold-clean", bound, "")
	held := store.TakePreferredAccountWithDispatch(bound.DBID, 0, nil, nil, DispatchPolicyStandard)
	if held != bound {
		t.Fatalf("held = %v", held)
	}

	selected, _, _ := store.WaitForSessionAvailableWithDispatchGuard(context.Background(), "no-borrow-hold-clean", 400*time.Millisecond, 0, nil, nil, DispatchPolicyStandard)
	if selected != nil {
		store.Release(selected)
		t.Fatalf("single-account pool must time out, got account %d", selected.DBID)
	}

	hub := store.schedulerAvailabilityHub()
	hub.mu.Lock()
	count, lanes, readyLanes, activeWaves := hub.count, len(hub.lanes), hub.ready.Len(), hub.activeWaves
	parked := 0
	for le := hub.ready.Front(); le != nil; le = le.Next() {
		parked += le.Value.(*availabilityLane).ready.Len()
	}
	hub.mu.Unlock()
	if count != 0 || lanes != 0 || readyLanes != 0 || parked != 0 || activeWaves != 0 {
		t.Fatalf("hub not clean after a timed-out hold: count=%d lanes=%d readyLanes=%d parked=%d activeWaves=%d", count, lanes, readyLanes, parked, activeWaves)
	}

	// 后续普通等待者必须能被 Release 唤醒；残留元素会让 Release 卡在 hub 锁上。
	store.SetSessionNoBorrow(false, time.Second)
	go func() {
		time.Sleep(100 * time.Millisecond)
		store.Release(held)
	}()
	started := time.Now()
	selected, _, _ = store.WaitForSessionAvailableWithDispatchGuard(context.Background(), "no-borrow-hold-clean", 3*time.Second, 0, nil, nil, DispatchPolicyStandard)
	elapsed := time.Since(started)
	if selected != bound {
		if selected != nil {
			store.Release(selected)
		}
		t.Fatalf("later waiter must be woken by Release, got %v after %s", selected, elapsed)
	}
	store.Release(selected)
	if elapsed > 2*time.Second {
		t.Fatalf("Release did not wake the later waiter promptly: %s", elapsed)
	}
}

// 不借用只约束 Codex 官方账号：绑定的是中转/Grok 等 relay-style 账号时沿用旧借用逻辑。
func TestNoBorrowSkipsRelayStyleBoundAccount(t *testing.T) {
	bound := &Account{DBID: 1, UpstreamType: UpstreamOpenAIResponses, BaseURL: "https://relay.example", APIKey: "sk-relay"}
	fallback := &Account{DBID: 2, AccessToken: "tok-2"}
	if !bound.IsRelayStyle() {
		t.Fatal("fixture must be relay-style")
	}
	store := &Store{accounts: []*Account{bound, fallback}, maxConcurrency: 1}
	store.SetSessionNoBorrow(true, time.Second)
	store.sessionNoBorrowHoldNS.Store(int64(150 * time.Millisecond))
	store.bindSessionAffinity("no-borrow-relay", bound, "")
	held := store.TakePreferredAccountWithDispatch(bound.DBID, 0, nil, nil, DispatchPolicyStandard)
	if held != bound {
		t.Fatalf("held = %v", held)
	}
	defer store.Release(held)

	selected, _, guard := store.NextForSessionWithDispatchGuard("no-borrow-relay", 0, nil, nil, DispatchPolicyStandard)
	if selected != fallback || !guard.PreservesExisting() {
		if selected != nil {
			store.Release(selected)
		}
		t.Fatalf("relay-bound session must borrow immediately: selected=%v guard=%+v", selected, guard)
	}
	store.ReleaseForSessionWithGuard(selected, "no-borrow-relay", guard)
	if stats := store.SessionBorrowStats(); stats.Held != 0 || stats.Borrowed != 1 {
		t.Fatalf("stats = %+v, want held 0 borrowed 1", stats)
	}

	// 等待路径同样不为 relay-style 绑定扣 hold。
	started := time.Now()
	selected, _, guard = store.WaitForSessionAvailableWithDispatchGuard(context.Background(), "no-borrow-relay", 2*time.Second, 0, nil, nil, DispatchPolicyStandard)
	elapsed := time.Since(started)
	if selected != fallback {
		if selected != nil {
			store.Release(selected)
		}
		t.Fatalf("relay-bound wait must borrow the fallback, got %v", selected)
	}
	store.ReleaseForSessionWithGuard(selected, "no-borrow-relay", guard)
	if elapsed >= 150*time.Millisecond {
		t.Fatalf("relay-bound wait waited for the hold: %s", elapsed)
	}
	if stats := store.SessionBorrowStats(); stats.Held != 0 || stats.Borrowed != 2 {
		t.Fatalf("stats = %+v, want held 0 borrowed 2", stats)
	}
}
