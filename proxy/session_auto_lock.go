package proxy

import (
	"context"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// 连续 500 自动锁定：以会话粘性键为身份，只统计官方 Codex 账号上最终完成的 500
// （中转账号、内部重试、内部子请求不计），达阈值写锁（DB + 内存），入口按键拒绝。
// 连击只在内存里，重启和保存设置都清零；锁没有 TTL，只有管理员解锁。

const (
	sessionAutoLockKeyContextKey = "codex2api.session_auto_lock.key"
	sessionAutoLockStreakCap     = 50000
	sessionAutoLockSource        = "automatic"
)

type sessionAutoLockStreak struct {
	count     int
	updatedAt time.Time
}

type sessionAutoLockState struct {
	mu            sync.Mutex
	streaks       map[string]*sessionAutoLockStreak
	locked        map[string]struct{}
	loaded        bool
	lockedTotal   atomic.Uint64
	unlockedTotal atomic.Uint64
}

var sessionAutoLock = newSessionAutoLockState()

func newSessionAutoLockState() *sessionAutoLockState {
	return &sessionAutoLockState{streaks: make(map[string]*sessionAutoLockStreak), locked: make(map[string]struct{})}
}

func resetSessionAutoLockForTest() {
	fresh := newSessionAutoLockState()
	sessionAutoLock.mu.Lock()
	sessionAutoLock.streaks, sessionAutoLock.locked, sessionAutoLock.loaded = fresh.streaks, fresh.locked, false
	sessionAutoLock.mu.Unlock()
	sessionAutoLock.lockedTotal.Store(0)
	sessionAutoLock.unlockedTotal.Store(0)
}

// ResetSessionAutoLockStreaks 保存设置时调用：未触发的连击全部清零，已有锁不动。
func ResetSessionAutoLockStreaks() {
	sessionAutoLock.mu.Lock()
	sessionAutoLock.streaks = make(map[string]*sessionAutoLockStreak)
	sessionAutoLock.mu.Unlock()
}

func (h *Handler) rememberSessionAutoLockKey(c *gin.Context, affinityKey string) {
	if c == nil {
		return
	}
	if key := strings.TrimSpace(affinityKey); key != "" {
		c.Set(sessionAutoLockKeyContextKey, key)
	}
}

func sessionAutoLockKeyFromContext(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if value, ok := c.Get(sessionAutoLockKeyContextKey); ok {
		if key, ok := value.(string); ok {
			return strings.TrimSpace(key)
		}
	}
	return ""
}

// ensureLoadedLocked 首次使用时从 DB 预热锁表；失败只打日志（下次再试）。
func (h *Handler) ensureSessionAutoLocksLoadedLocked() {
	if sessionAutoLock.loaded || h == nil || h.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	keys, err := h.db.ListSessionAutoLockKeys(ctx)
	if err != nil {
		log.Printf("[SESSION-AUTO-LOCK] 预热锁表失败: %v", err)
		return
	}
	for _, key := range keys {
		sessionAutoLock.locked[key] = struct{}{}
	}
	sessionAutoLock.loaded = true
}

func sessionAutoLockError() *api.APIError {
	return api.NewAPIErrorWithDetails(api.ErrorCode("session_blacklisted"), "该会话因连续上游错误已被锁定，请新建对话；如需恢复请管理员解锁。", api.ErrorTypeInvalidRequest, map[string]any{"retry": "stop"})
}

// checkSessionAutoLock 入口检查：命中锁即拒绝，与账号类型无关（计数阶段已排除中转）。
func (h *Handler) checkSessionAutoLock(c *gin.Context, affinityKey string) *api.APIError {
	key := strings.TrimSpace(affinityKey)
	if key == "" {
		return nil
	}
	sessionAutoLock.mu.Lock()
	h.ensureSessionAutoLocksLoadedLocked()
	_, locked := sessionAutoLock.locked[key]
	sessionAutoLock.mu.Unlock()
	if !locked {
		return nil
	}
	return sessionAutoLockError()
}

func sessionIDPrefixFromAffinityKey(key string) string {
	session := key
	if idx := strings.Index(key, "::api-key:"); idx >= 0 {
		session = key[:idx]
	}
	if len(session) > 12 {
		session = session[:12]
	}
	return session
}

func apiKeyIDFromAffinityKey(key string) int64 {
	idx := strings.Index(key, "::api-key:")
	if idx < 0 {
		return 0
	}
	var id int64
	for _, ch := range key[idx+len("::api-key:"):] {
		if ch < '0' || ch > '9' {
			break
		}
		id = id*10 + int64(ch-'0')
	}
	return id
}

// observeSessionAutoLock 在 logUsageForRequest 里调用：按最终结果维护连击并落锁。
func (h *Handler) observeSessionAutoLock(c *gin.Context, input *database.UsageLogInput) {
	if h == nil || input == nil {
		return
	}
	settings := CurrentRuntimeSettings()
	if !settings.CodexSessionAutoLockEnabled {
		return
	}
	key := sessionAutoLockKeyFromContext(c)
	if key == "" || input.IsRetryAttempt || strings.TrimSpace(input.InternalReason) != "" || input.AccountID <= 0 {
		return
	}
	if h.store == nil {
		return
	}
	account := h.store.FindByID(input.AccountID)
	if account == nil || account.IsRelayStyle() {
		return
	}
	threshold := database.NormalizeSessionAutoLockThreshold(settings.CodexSessionAutoLockThreshold)
	now := time.Now()
	sessionAutoLock.mu.Lock()
	if input.StatusCode != 500 {
		delete(sessionAutoLock.streaks, key)
		sessionAutoLock.mu.Unlock()
		return
	}
	streak := sessionAutoLock.streaks[key]
	if streak == nil {
		if len(sessionAutoLock.streaks) >= sessionAutoLockStreakCap {
			evictOldestSessionAutoLockStreaksLocked(len(sessionAutoLock.streaks) / 10)
		}
		streak = &sessionAutoLockStreak{}
		sessionAutoLock.streaks[key] = streak
	}
	streak.count++
	streak.updatedAt = now
	if streak.count < threshold {
		sessionAutoLock.mu.Unlock()
		return
	}
	delete(sessionAutoLock.streaks, key)
	_, already := sessionAutoLock.locked[key]
	sessionAutoLock.locked[key] = struct{}{}
	sessionAutoLock.mu.Unlock()
	if already {
		return
	}
	sessionAutoLock.lockedTotal.Add(1)
	log.Printf("[SESSION-AUTO-LOCK] locked session=%s account=%d threshold=%d error=%q", sessionIDPrefixFromAffinityKey(key), input.AccountID, threshold, strings.TrimSpace(input.ErrorMessage))
	if h.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, _, err := h.db.InsertSessionAutoLock(ctx, database.SessionAutoLockInput{
		SessionKey: key, SessionIDPrefix: sessionIDPrefixFromAffinityKey(key), APIKeyID: apiKeyIDFromAffinityKey(key),
		AccountID: input.AccountID, ErrorMessage: input.ErrorMessage, Threshold: threshold,
	}); err != nil {
		log.Printf("[SESSION-AUTO-LOCK] 写锁失败（内存锁仍生效）: %v", err)
	}
}

func evictOldestSessionAutoLockStreaksLocked(n int) {
	type entry struct {
		key string
		at  time.Time
	}
	entries := make([]entry, 0, len(sessionAutoLock.streaks))
	for key, streak := range sessionAutoLock.streaks {
		entries = append(entries, entry{key, streak.updatedAt})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].at.Before(entries[j].at) })
	if n < 1 {
		n = 1
	}
	for i := 0; i < n && i < len(entries); i++ {
		delete(sessionAutoLock.streaks, entries[i].key)
	}
}

// UnlockSessionAutoLock 管理员解锁后从内存移除（DB 行由 admin 层删除）。
func (h *Handler) UnlockSessionAutoLock(key string) {
	key = strings.TrimSpace(key)
	sessionAutoLock.mu.Lock()
	_, existed := sessionAutoLock.locked[key]
	delete(sessionAutoLock.locked, key)
	delete(sessionAutoLock.streaks, key)
	sessionAutoLock.mu.Unlock()
	if existed {
		sessionAutoLock.unlockedTotal.Add(1)
	}
}

type SessionGuardAutoLockStatus struct {
	Enabled       bool   `json:"enabled"`
	Threshold     int    `json:"threshold"`
	ActiveLocks   uint64 `json:"active_locks"`
	LockedTotal   uint64 `json:"locked_total"`
	UnlockedTotal uint64 `json:"unlocked_total"`
	StreakEntries uint64 `json:"streak_entries"`
}

func sessionAutoLockSnapshot(h *Handler) SessionGuardAutoLockStatus {
	settings := CurrentRuntimeSettings()
	sessionAutoLock.mu.Lock()
	if h != nil {
		h.ensureSessionAutoLocksLoadedLocked()
	}
	active, streaks := uint64(len(sessionAutoLock.locked)), uint64(len(sessionAutoLock.streaks))
	sessionAutoLock.mu.Unlock()
	return SessionGuardAutoLockStatus{
		Enabled: settings.CodexSessionAutoLockEnabled, Threshold: database.NormalizeSessionAutoLockThreshold(settings.CodexSessionAutoLockThreshold),
		ActiveLocks: active, LockedTotal: sessionAutoLock.lockedTotal.Load(), UnlockedTotal: sessionAutoLock.unlockedTotal.Load(), StreakEntries: streaks,
	}
}
