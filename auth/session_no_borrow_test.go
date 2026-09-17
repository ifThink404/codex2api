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
