package auth

import (
	"testing"
	"time"
)

// (e) 不借用策略是会话防护的一环：绑定账号把 session_guards_policy 设成 off 之后，
// 它的会话回到旧的容量溢出借用语义，不再被扣住等待。
func TestSessionNoBorrowSkipsAccountsWithGuardsOff(t *testing.T) {
	guarded := &Account{DBID: 1, AccessToken: "tok-1"}
	off := &Account{DBID: 2, AccessToken: "tok-2", SessionGuardsPolicy: SessionGuardsPolicyOff}
	relay := &Account{DBID: 3, UpstreamType: UpstreamOpenAIResponses, BaseURL: "https://relay.example", APIKey: "sk"}
	store := &Store{accounts: []*Account{guarded, off, relay}, maxConcurrency: 1}

	if !store.sessionNoBorrowAppliesTo(guarded.DBID) {
		t.Fatal("an official account with the default policy must stay under the no-borrow hold")
	}
	if store.sessionNoBorrowAppliesTo(off.DBID) {
		t.Fatal("session_guards_policy=off must release the account from the no-borrow hold")
	}
	if store.sessionNoBorrowAppliesTo(relay.DBID) || store.sessionNoBorrowAppliesTo(0) {
		t.Fatal("relay-style and unknown accounts must stay outside the no-borrow hold")
	}
}

// 行为面：绑定到 policy=off 的账号、该账号并发已满时，选号必须借用别的账号
// 而不是返回 nil 让调用方去等。
func TestNoBorrowSpillsOverForGuardsOffBinding(t *testing.T) {
	bound := &Account{DBID: 1, AccessToken: "tok-1", SessionGuardsPolicy: SessionGuardsPolicyOff}
	fallback := &Account{DBID: 2, AccessToken: "tok-2"}
	store := &Store{accounts: []*Account{bound, fallback}, maxConcurrency: 1}
	store.SetSessionNoBorrow(true, 5*time.Second)
	store.bindSessionAffinity("guards-off-borrow", bound, "")

	held := store.TakePreferredAccountWithDispatch(bound.DBID, 0, nil, nil, DispatchPolicyStandard)
	if held != bound {
		t.Fatalf("held = %v, want the bound account", held)
	}
	defer store.Release(held)

	selected, _, guard := store.NextForSessionWithDispatchGuard("guards-off-borrow", 0, nil, nil, DispatchPolicyStandard)
	if selected != fallback {
		if selected != nil {
			store.Release(selected)
		}
		t.Fatalf("guards-off binding must spill over at capacity, got %v", selected)
	}
	store.ReleaseForSessionWithGuard(selected, "guards-off-borrow", guard)
	if stats := store.SessionBorrowStats(); stats.Held != 0 || stats.Borrowed != 1 {
		t.Fatalf("stats = %+v, want held 0 borrowed 1", stats)
	}
}
