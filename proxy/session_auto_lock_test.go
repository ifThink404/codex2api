package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func newAutoLockTestHandler(t *testing.T) (*Handler, *auth.Account, *auth.Account) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "autolock.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 4})
	official := &auth.Account{DBID: 244, AccessToken: "tok"}
	relay := &auth.Account{DBID: 300, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://relay.example", APIKey: "sk-relay"}
	if !relay.IsRelayStyle() {
		t.Fatal("test fixture relay account must satisfy IsRelayStyle() for the exemption test to be meaningful")
	}
	store.AddAccount(official)
	store.AddAccount(relay)
	resetSessionAutoLockForTest()
	previous := CurrentRuntimeSettings()
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings {
		s.CodexSessionAutoLockEnabled = true
		s.CodexSessionAutoLockThreshold = 3
		return s
	})
	t.Cleanup(func() { ApplyRuntimeSettings(previous); resetSessionAutoLockForTest() })
	return &Handler{store: store, db: db}, official, relay
}

func autoLockTestContext(h *Handler, key string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	h.rememberSessionAutoLockKey(c, key)
	return c
}

func TestSessionAutoLockLocksAfterThresholdAndRejects(t *testing.T) {
	h, official, _ := newAutoLockTestHandler(t)
	key := "thread-1::api-key:9"
	for i := 0; i < 2; i++ {
		h.observeSessionAutoLock(autoLockTestContext(h, key), &database.UsageLogInput{StatusCode: 500, AccountID: official.DBID, ErrorMessage: "server_is_overloaded"})
	}
	if err := h.checkSessionAutoLock(autoLockTestContext(h, key), key); err != nil {
		t.Fatalf("two failures must not lock: %v", err)
	}
	h.observeSessionAutoLock(autoLockTestContext(h, key), &database.UsageLogInput{StatusCode: 500, AccountID: official.DBID, ErrorMessage: "server_is_overloaded"})
	err := h.checkSessionAutoLock(autoLockTestContext(h, key), key)
	if err == nil || string(err.Code) != "session_blacklisted" {
		t.Fatalf("third failure must lock, got %v", err)
	}
	if details, _ := err.Details.(map[string]any); details == nil || details["retry"] != "stop" {
		t.Fatalf("details = %#v", err.Details)
	}
	locks, listErr := h.db.ListSessionAutoLocks(c0(), 10)
	if listErr != nil || len(locks) != 1 || locks[0].SessionKey != key || locks[0].SessionIDPrefix != "thread-1" || locks[0].APIKeyID != 9 || locks[0].AccountID != 244 {
		t.Fatalf("persisted lock = %#v err=%v", locks, listErr)
	}
	status := sessionAutoLockSnapshot(h)
	if status.ActiveLocks != 1 || status.LockedTotal != 1 {
		t.Fatalf("status = %+v", status)
	}
	h.UnlockSessionAutoLock(key)
	if err := h.checkSessionAutoLock(autoLockTestContext(h, key), key); err != nil {
		t.Fatalf("unlocked session must pass: %v", err)
	}
	if status := sessionAutoLockSnapshot(h); status.ActiveLocks != 0 || status.UnlockedTotal != 1 {
		t.Fatalf("status after unlock = %+v", status)
	}
}

func TestSessionAutoLockResetsOnSuccessRetryInternalRelayAndDisabled(t *testing.T) {
	h, official, relay := newAutoLockTestHandler(t)
	key := "thread-2::api-key:9"
	fail := &database.UsageLogInput{StatusCode: 500, AccountID: official.DBID}
	h.observeSessionAutoLock(autoLockTestContext(h, key), fail)
	h.observeSessionAutoLock(autoLockTestContext(h, key), fail)
	h.observeSessionAutoLock(autoLockTestContext(h, key), &database.UsageLogInput{StatusCode: 200, AccountID: official.DBID})
	h.observeSessionAutoLock(autoLockTestContext(h, key), fail)
	h.observeSessionAutoLock(autoLockTestContext(h, key), fail)
	if err := h.checkSessionAutoLock(autoLockTestContext(h, key), key); err != nil {
		t.Fatalf("a success must reset the streak: %v", err)
	}
	for i := 0; i < 5; i++ {
		h.observeSessionAutoLock(autoLockTestContext(h, key), &database.UsageLogInput{StatusCode: 500, AccountID: official.DBID, IsRetryAttempt: true})
		h.observeSessionAutoLock(autoLockTestContext(h, key), &database.UsageLogInput{StatusCode: 500, AccountID: official.DBID, InternalReason: "title"})
		h.observeSessionAutoLock(autoLockTestContext(h, key), &database.UsageLogInput{StatusCode: 500, AccountID: relay.DBID})
	}
	if err := h.checkSessionAutoLock(autoLockTestContext(h, key), key); err != nil {
		t.Fatalf("retry/internal/relay results must not count: %v", err)
	}
	if got := sessionAutoLockSnapshot(h).StreakEntries; got != 1 {
		t.Fatalf("streak entries = %d, want 1 (the two real failures)", got)
	}
	ResetSessionAutoLockStreaks()
	if got := sessionAutoLockSnapshot(h).StreakEntries; got != 0 {
		t.Fatalf("reset must clear streaks, got %d", got)
	}
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexSessionAutoLockEnabled = false; return s })
	for i := 0; i < 5; i++ {
		h.observeSessionAutoLock(autoLockTestContext(h, key), fail)
	}
	if err := h.checkSessionAutoLock(autoLockTestContext(h, key), key); err != nil {
		t.Fatalf("disabled guard must never lock: %v", err)
	}
	if err := h.checkSessionAutoLock(autoLockTestContext(h, ""), ""); err != nil {
		t.Fatalf("empty key must pass: %v", err)
	}
}

func TestSessionAutoLockWarmsExistingLocksFromDatabase(t *testing.T) {
	h, _, _ := newAutoLockTestHandler(t)
	if _, _, err := h.db.InsertSessionAutoLock(c0(), database.SessionAutoLockInput{SessionKey: "old::api-key:1", SessionIDPrefix: "old", Threshold: 3}); err != nil {
		t.Fatal(err)
	}
	resetSessionAutoLockForTest()
	if err := h.checkSessionAutoLock(autoLockTestContext(h, "old::api-key:1"), "old::api-key:1"); err == nil {
		t.Fatal("lock persisted in the database must be enforced after a restart")
	}
}

func c0() context.Context { return context.Background() }
