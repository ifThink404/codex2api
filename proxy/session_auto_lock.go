package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// 连续 500 自动锁定：以会话粘性键为身份，只统计官方 Codex 账号上最终完成的 500
// （中转账号、内部重试、内部子请求不计），达阈值写锁（DB + 内存），入口按键拒绝。
// 连击只在内存里，重启和保存设置都清零；锁没有 TTL，只有管理员解锁。
//
// 锁表预热是异步、单飞的：请求路径只在已持有的互斥锁下做一次内存判断，从不等待 DB。
// 进程启动后第一次调用 checkSessionAutoLock/sessionAutoLockSnapshot 会在后台触发一次
// 加载，成功后（通常是首个请求之后的几毫秒）DB 里持久化的锁才开始在内存里生效；加载
// 失败只记日志，至多每 30 秒重试一次，不会让请求路径卡在不可用的 DB 上。

const (
	sessionAutoLockKeyContextKey = "codex2api.session_auto_lock.key"
	sessionAutoLockStreakCap     = 50000
	sessionAutoLockSource        = "automatic"
	// sessionAutoLockKeyMaxRunes 是 session_key 列宽（VARCHAR(255) 按字符计）。
	sessionAutoLockKeyMaxRunes = 255
	// sessionAutoLockWarmupRetry 是锁表预热失败后的最小重试间隔，避免不可用的 DB
	// 拖慢每一次请求路径上的判断。
	sessionAutoLockWarmupRetry = 30 * time.Second
)

type sessionAutoLockStreak struct {
	count     int
	updatedAt time.Time
}

type sessionAutoLockState struct {
	mu      sync.Mutex
	streaks map[string]*sessionAutoLockStreak
	locked  map[string]struct{}
	loaded  bool
	// loading 为真时已有一个预热 goroutine 在途，避免重复并发加载。
	loading bool
	// lastLoadAttempt 记录最近一次触发预热的时间，配合 sessionAutoLockWarmupRetry 做冷却。
	lastLoadAttempt time.Time
	// pendingUnlocks 记录预热完成前被管理员解锁的键：加载结果落地时会跳过这些键，
	// 防止一次仍在途的旧查询把刚解的锁又写回内存。loaded 之后清空、不再使用。
	pendingUnlocks map[string]struct{}
	lockedTotal    atomic.Uint64
	unlockedTotal  atomic.Uint64
}

var sessionAutoLock = newSessionAutoLockState()

func newSessionAutoLockState() *sessionAutoLockState {
	return &sessionAutoLockState{
		streaks:        make(map[string]*sessionAutoLockStreak),
		locked:         make(map[string]struct{}),
		pendingUnlocks: make(map[string]struct{}),
	}
}

func resetSessionAutoLockForTest() {
	fresh := newSessionAutoLockState()
	sessionAutoLock.mu.Lock()
	sessionAutoLock.streaks, sessionAutoLock.locked, sessionAutoLock.loaded = fresh.streaks, fresh.locked, false
	sessionAutoLock.loading, sessionAutoLock.lastLoadAttempt, sessionAutoLock.pendingUnlocks = false, time.Time{}, fresh.pendingUnlocks
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

// canonicalSessionAutoLockKey 把会话键收敛成 DB 能原样存回的形状。会话键里的会话 ID
// 直接来自客户端（Session-Id / Conversation-Id / Idempotency-Key / prompt_cache_key），
// 长度不设上限，而 session_key 列只有 255 字符：内存留全值、DB 留截断值的话，管理员
// 解锁会静默失效（删掉了行、内存锁还在）、重启预热也永远对不上号，且前 255 字符相同的
// 两个会话会在 UNIQUE 索引上撞车。超长键一律换成 SHA-256，保证内存 / DB / 预热 / 解锁
// 看到的是同一把键。
func canonicalSessionAutoLockKey(key string) string {
	if utf8.RuneCountInString(key) <= sessionAutoLockKeyMaxRunes {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	return "h:" + hex.EncodeToString(sum[:])
}

// rememberSessionAutoLockKey 是会话键进入自动锁定子系统的唯一入口：在这里收敛一次，
// 计数、锁判断、落库、预热合并、管理员解锁便都用同一把键。
func (h *Handler) rememberSessionAutoLockKey(c *gin.Context, affinityKey string) {
	if c == nil {
		return
	}
	if key := canonicalSessionAutoLockKey(strings.TrimSpace(affinityKey)); key != "" {
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

// kickSessionAutoLockWarmup 由已持有 sessionAutoLock.mu 的调用方触发：至多每
// sessionAutoLockWarmupRetry 尝试一次后台预热，本身从不等待 DB，请求路径不会被卡住。
func (h *Handler) kickSessionAutoLockWarmup() {
	if sessionAutoLock.loaded || sessionAutoLock.loading || h == nil || h.db == nil {
		return
	}
	if time.Since(sessionAutoLock.lastLoadAttempt) < sessionAutoLockWarmupRetry {
		return
	}
	sessionAutoLock.loading = true
	sessionAutoLock.lastLoadAttempt = time.Now()
	go h.loadSessionAutoLocks()
}

// loadSessionAutoLocks 在后台 goroutine 里跑：查询期间不持锁，查完再夺锁写入结果，
// 避免 DB 慢/不可用时拖住任何持有 sessionAutoLock.mu 的请求路径。
func (h *Handler) loadSessionAutoLocks() {
	// 单飞标记用 defer 归位：查询或合并里一旦 panic，loading 留在 true 就再也没有
	// 预热会被触发，进程直到重启为止都不认 DB 里的锁。
	defer func() {
		sessionAutoLock.mu.Lock()
		sessionAutoLock.loading = false
		sessionAutoLock.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	keys, err := h.db.ListSessionAutoLockKeys(ctx)
	if err != nil {
		log.Printf("[SESSION-AUTO-LOCK] 预热锁表失败: %v", err)
		return
	}
	sessionAutoLock.mu.Lock()
	for _, key := range keys {
		if _, skip := sessionAutoLock.pendingUnlocks[key]; skip {
			continue
		}
		sessionAutoLock.locked[key] = struct{}{}
	}
	sessionAutoLock.pendingUnlocks = make(map[string]struct{})
	sessionAutoLock.loaded = true
	sessionAutoLock.mu.Unlock()
}

func sessionAutoLockError() *api.APIError {
	return api.NewAPIErrorWithDetails(api.ErrorCode("session_blacklisted"), "该会话因连续上游错误已被锁定，请新建对话；如需恢复请管理员解锁。", api.ErrorTypeInvalidRequest, map[string]any{"retry": "stop"})
}

// checkSessionAutoLock 入口检查：命中锁即拒绝，与账号类型无关（计数阶段已排除中转）。
// 键优先取 rememberSessionAutoLockKey 收敛好的那把；没有（独立调用）时就地收敛，
// 两条路都落在 canonicalSessionAutoLockKey 的同一个结果上。
func (h *Handler) checkSessionAutoLock(c *gin.Context, affinityKey string) *api.APIError {
	key := sessionAutoLockKeyFromContext(c)
	if key == "" {
		key = canonicalSessionAutoLockKey(strings.TrimSpace(affinityKey))
	}
	if key == "" {
		return nil
	}
	sessionAutoLock.mu.Lock()
	h.kickSessionAutoLockWarmup()
	_, locked := sessionAutoLock.locked[key]
	sessionAutoLock.mu.Unlock()
	if !locked {
		return nil
	}
	return sessionAutoLockError()
}

// sessionIDPrefixFromAffinityKey 截取会话前缀写入 DB 的 session_id_prefix 列（宽度 32，
// 这里保守取 12）。affinityID 来自客户端、未必是 ASCII，按字节切会切碎多字节 rune，
// 落到 PostgreSQL 上会被拒绝导致锁只留在内存里；这里按 rune 计数截断。
func sessionIDPrefixFromAffinityKey(key string) string {
	session := key
	if idx := strings.Index(key, "::api-key:"); idx >= 0 {
		session = key[:idx]
	}
	count := 0
	for i := range session {
		count++
		if count > 12 {
			session = session[:i]
			break
		}
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

// UnlockSessionAutoLock 管理员解锁后从内存移除（DB 行由 admin 层删除，调用方只在 DB
// 删除成功后才会调用这里，因此计数无条件累加）。预热尚未完成时，把键记进
// pendingUnlocks，防止一次仍在途的旧查询把刚解的锁又写回内存。
func (h *Handler) UnlockSessionAutoLock(key string) {
	key = strings.TrimSpace(key)
	sessionAutoLock.mu.Lock()
	delete(sessionAutoLock.locked, key)
	delete(sessionAutoLock.streaks, key)
	if !sessionAutoLock.loaded {
		sessionAutoLock.pendingUnlocks[key] = struct{}{}
	}
	sessionAutoLock.mu.Unlock()
	sessionAutoLock.unlockedTotal.Add(1)
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
	h.kickSessionAutoLockWarmup()
	active, streaks := uint64(len(sessionAutoLock.locked)), uint64(len(sessionAutoLock.streaks))
	sessionAutoLock.mu.Unlock()
	return SessionGuardAutoLockStatus{
		Enabled: settings.CodexSessionAutoLockEnabled, Threshold: database.NormalizeSessionAutoLockThreshold(settings.CodexSessionAutoLockThreshold),
		ActiveLocks: active, LockedTotal: sessionAutoLock.lockedTotal.Load(), UnlockedTotal: sessionAutoLock.unlockedTotal.Load(), StreakEntries: streaks,
	}
}
