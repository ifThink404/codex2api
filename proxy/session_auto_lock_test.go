package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
	key := "old::api-key:1"
	// The warm-up is async: the first call only kicks it off in the background.
	h.checkSessionAutoLock(autoLockTestContext(h, key), key)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := h.checkSessionAutoLock(autoLockTestContext(h, key), key); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("lock persisted in the database must be enforced after a restart")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSessionAutoLockWarmupFailureDoesNotBlockRequests 验证 GitNexus 评审发现 1 的修复：
// DB 不可用时预热失败绝不能让请求路径卡住，且失败后至少 sessionAutoLockWarmupRetry
// 之内不会重试（单飞冷却）。不复用 newAutoLockTestHandler：那个 helper 会在 t.Cleanup
// 里再 Close 一次数据库，而 database.DB.Close 对已关闭的连接不是幂等的（会 panic）。
func TestSessionAutoLockWarmupFailureDoesNotBlockRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "autolock-closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	resetSessionAutoLockForTest()
	previous := CurrentRuntimeSettings()
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings {
		s.CodexSessionAutoLockEnabled = true
		s.CodexSessionAutoLockThreshold = 3
		return s
	})
	t.Cleanup(func() { ApplyRuntimeSettings(previous); resetSessionAutoLockForTest() })
	h := &Handler{db: db}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	key := "closed-db::api-key:1"

	start := time.Now()
	for i := 0; i < 50; i++ {
		if err := h.checkSessionAutoLock(autoLockTestContext(h, key), key); err != nil {
			t.Fatalf("a closed DB must never surface as a lock: %v", err)
		}
	}
	if elapsed := time.Since(start); elapsed >= 500*time.Millisecond {
		t.Fatalf("50 checks against a closed DB took %v, want < 500ms (must not block on DB)", elapsed)
	}

	// Let the async warm-up attempt (which will fail against the closed DB) settle.
	deadline := time.Now().Add(2 * time.Second)
	for {
		sessionAutoLock.mu.Lock()
		loading := sessionAutoLock.loading
		sessionAutoLock.mu.Unlock()
		if !loading {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("warm-up goroutine never settled loading back to false")
		}
		time.Sleep(20 * time.Millisecond)
	}
	sessionAutoLock.mu.Lock()
	firstAttempt := sessionAutoLock.lastLoadAttempt
	sessionAutoLock.mu.Unlock()
	if firstAttempt.IsZero() {
		t.Fatal("a warm-up attempt should have been kicked off")
	}

	start = time.Now()
	for i := 0; i < 50; i++ {
		if err := h.checkSessionAutoLock(autoLockTestContext(h, key), key); err != nil {
			t.Fatalf("a closed DB must never surface as a lock: %v", err)
		}
	}
	if elapsed := time.Since(start); elapsed >= 500*time.Millisecond {
		t.Fatalf("second burst of 50 checks took %v, want < 500ms", elapsed)
	}

	sessionAutoLock.mu.Lock()
	secondAttempt, stillLoading := sessionAutoLock.lastLoadAttempt, sessionAutoLock.loading
	sessionAutoLock.mu.Unlock()
	if stillLoading {
		t.Fatal("second burst must not be left mid-load: the cooldown should have skipped a new attempt")
	}
	if !secondAttempt.Equal(firstAttempt) {
		t.Fatalf("second burst is within the 30s cooldown and must not retry: first=%v second=%v", firstAttempt, secondAttempt)
	}
}

func TestSessionAutoLockDisabledKeepsExistingLocks(t *testing.T) {
	h, official, _ := newAutoLockTestHandler(t)
	key := "thread-disable::api-key:9"
	for i := 0; i < 3; i++ {
		h.observeSessionAutoLock(autoLockTestContext(h, key), &database.UsageLogInput{StatusCode: 500, AccountID: official.DBID})
	}
	if err := h.checkSessionAutoLock(autoLockTestContext(h, key), key); err == nil {
		t.Fatal("three failures must lock while the guard is enabled")
	}
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexSessionAutoLockEnabled = false; return s })
	if err := h.checkSessionAutoLock(autoLockTestContext(h, key), key); err == nil {
		t.Fatal("disabling the guard must not release an existing lock")
	}
}

func TestSessionIDPrefixFromAffinityKeyIsRuneSafe(t *testing.T) {
	cjkKey := strings.Repeat("会", 20) + "::api-key:9"
	prefix := sessionIDPrefixFromAffinityKey(cjkKey)
	if !utf8.ValidString(prefix) {
		t.Fatalf("prefix is not valid UTF-8: %q", prefix)
	}
	if got := utf8.RuneCountInString(prefix); got != 12 {
		t.Fatalf("rune count = %d, want 12 (prefix=%q)", got, prefix)
	}
	if id := apiKeyIDFromAffinityKey(cjkKey); id != 9 {
		t.Fatalf("api key id = %d, want 9", id)
	}

	asciiKey := "abcdef12-3456-7890-abcd-ef1234567890::api-key:9"
	if got := sessionIDPrefixFromAffinityKey(asciiKey); got != "abcdef12-345" {
		t.Fatalf("ascii prefix = %q, want first 12 characters", got)
	}
	if id := apiKeyIDFromAffinityKey(asciiKey); id != 9 {
		t.Fatalf("api key id = %d, want 9", id)
	}
}

func c0() context.Context { return context.Background() }

// 超长会话键：会话 ID 直接来自客户端（Session-Id / prompt_cache_key 都没有长度上限），
// 而 session_key 列只有 255 字符。键一旦在内存和 DB 之间长得不一样，管理员解锁就变成
// 静默失败（DB 行删了、内存锁还在，只能重启），重启预热也永远对不上号。
func TestSessionAutoLockCanonicalizesOverlongKeys(t *testing.T) {
	h, official, _ := newAutoLockTestHandler(t)
	raw := strings.Repeat("会", 300) + "::api-key:9"
	canonical := canonicalSessionAutoLockKey(raw)
	if canonical == raw || !strings.HasPrefix(canonical, "h:") || len(canonical) != len("h:")+64 {
		t.Fatalf("overlong key must collapse to a hash, got %q", canonical)
	}
	if got := canonicalSessionAutoLockKey(canonical); got != canonical {
		t.Fatalf("canonicalisation must be idempotent, got %q", got)
	}
	if short := "thread-1::api-key:9"; canonicalSessionAutoLockKey(short) != short {
		t.Fatal("keys inside the column width must pass through unchanged")
	}
	if boundary := strings.Repeat("会", 255); canonicalSessionAutoLockKey(boundary) != boundary {
		t.Fatal("255 runes is the column width, not 255 bytes: it must pass through unchanged")
	}

	for i := 0; i < 3; i++ {
		h.observeSessionAutoLock(autoLockTestContext(h, raw), &database.UsageLogInput{StatusCode: 500, AccountID: official.DBID, ErrorMessage: "server_is_overloaded"})
	}
	if err := h.checkSessionAutoLock(autoLockTestContext(h, raw), raw); err == nil {
		t.Fatal("an overlong key must still lock once the threshold is reached")
	}
	locks, err := h.db.ListSessionAutoLocks(c0(), 10)
	if err != nil || len(locks) != 1 {
		t.Fatalf("persisted locks = %#v err=%v", locks, err)
	}
	if locks[0].SessionKey != canonical {
		t.Fatalf("persisted key = %q, want the canonical key %q (a clamped key can never round-trip)", locks[0].SessionKey, canonical)
	}

	// 重启：内存清空后由预热从 DB 读回，同一个会话必须仍然被拦住。
	resetSessionAutoLockForTest()
	h.loadSessionAutoLocks()
	if err := h.checkSessionAutoLock(autoLockTestContext(h, raw), raw); err == nil {
		t.Fatal("the persisted lock must be re-enforced after a restart")
	}

	// 管理员解锁用的是 DB 行里的键（admin/session_locks.go），必须命中同一个内存锁。
	h.UnlockSessionAutoLock(locks[0].SessionKey)
	if err := h.checkSessionAutoLock(autoLockTestContext(h, raw), raw); err != nil {
		t.Fatalf("admin unlock must release the session: %v", err)
	}
	if status := sessionAutoLockSnapshot(h); status.ActiveLocks != 0 {
		t.Fatalf("status after unlock = %+v", status)
	}
}
