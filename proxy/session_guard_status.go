package proxy

import (
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

type SessionGuardSettingsStatus struct {
	TurnStateStrict                bool `json:"turn_state_strict"`
	NoBorrowEnabled                bool `json:"no_borrow_enabled"`
	NoBorrowHoldSeconds            int  `json:"no_borrow_hold_seconds"`
	InitialSessionAdmissionEnabled bool `json:"initial_session_admission_enabled"`
	InitialSessionMaxAgeSeconds    int  `json:"initial_session_max_age_seconds"`
}

type SessionGuardTurnStateStatus struct {
	Totals   SessionGuardTurnStateCounters  `json:"totals"`
	Accounts []SessionGuardTurnStateAccount `json:"accounts"`
	Vault    SessionGuardVaultCounters      `json:"vault"`
}

type SessionGuardInitialStatus struct {
	RecentHour SessionGuardInitialSummary `json:"recent_hour"`
	SinceStart SessionGuardInitialSummary `json:"since_start"`
}

// SessionGuardPromptPolicyStatus 统计账号级 prompt 策略的两种结局：豁免放行、
// 选号后仍然拦截。进程内计数，重启清零。
type SessionGuardPromptPolicyStatus struct {
	Exempted              uint64 `json:"exempted"`
	BlockedAfterSelection uint64 `json:"blocked_after_selection"`
}

type SessionGuardStatus struct {
	StartedAt      string                         `json:"started_at"`
	Settings       SessionGuardSettingsStatus     `json:"settings"`
	TurnState      SessionGuardTurnStateStatus    `json:"turn_state"`
	Borrow         auth.SessionBorrowStats        `json:"borrow"`
	InitialSession SessionGuardInitialStatus      `json:"initial_session"`
	AutoLock       SessionGuardAutoLockStatus     `json:"auto_lock"`
	PromptPolicy   SessionGuardPromptPolicyStatus `json:"prompt_policy"`
}

// SessionGuardStatusSnapshot 供 /api/admin/runtime 使用：全部是进程内计数，重启清零。
func SessionGuardStatusSnapshot(store *auth.Store) SessionGuardStatus {
	settings := CurrentRuntimeSettings()
	totals, accounts := sessionGuardTurnStateSnapshot()
	if accounts == nil {
		accounts = []SessionGuardTurnStateAccount{}
	}
	recent, since := sessionGuardInitialSnapshot(time.Now())
	exempted, blockedAfterSelection := PromptPolicyCounters()
	status := SessionGuardStatus{
		StartedAt: sessionGuardStartedAt().Format(time.RFC3339),
		Settings: SessionGuardSettingsStatus{
			TurnStateStrict:                settings.CodexTurnStateStrict,
			InitialSessionAdmissionEnabled: settings.CodexInitialSessionAdmissionEnabled,
			InitialSessionMaxAgeSeconds:    database.NormalizeCodexInitialSessionMaxAgeSeconds(settings.CodexInitialSessionMaxAgeSeconds),
			NoBorrowHoldSeconds:            20,
		},
		TurnState:      SessionGuardTurnStateStatus{Totals: totals, Accounts: accounts, Vault: turnStateVaultCountersSnapshot()},
		InitialSession: SessionGuardInitialStatus{RecentHour: recent, SinceStart: since},
		PromptPolicy:   SessionGuardPromptPolicyStatus{Exempted: exempted, BlockedAfterSelection: blockedAfterSelection},
		// 自动锁定的开关、阈值与计数全在进程内，不需要 *Handler：只有 DB 预热要
		// Handler。这里一并填上，否则没有 Handler 的调用方（admin runtime-status 在
		// 未接 auth cache proxy 时走的就是这条分支）会把 AutoLock 留成零值，运行状态
		// 报 enabled=false，而 observeSessionAutoLock 读的是同一份设置、照样在落锁。
		AutoLock: sessionAutoLockSnapshot(nil),
	}
	if store != nil {
		status.Settings.NoBorrowEnabled = store.SessionNoBorrowEnabled()
		status.Settings.NoBorrowHoldSeconds = int(store.SessionNoBorrowHold() / time.Second)
		status.Borrow = store.SessionBorrowStats()
	}
	return status
}

// SessionGuardStatusSnapshotForHandler 在 SessionGuardStatusSnapshot 之上补上自动锁定
// 计数：只有 *Handler 才能触发锁表的 DB 预热（没有预热时 active_locks 在重启后要等
// 首个请求才回来）。store 与 h 分开传：调用方（admin）即便还没接上 proxy Handler，
// 也要保留自己 Store 的借用统计，两者都可以为 nil。
func SessionGuardStatusSnapshotForHandler(store *auth.Store, h *Handler) SessionGuardStatus {
	if store == nil && h != nil {
		store = h.store
	}
	status := SessionGuardStatusSnapshot(store)
	status.AutoLock = sessionAutoLockSnapshot(h)
	return status
}
