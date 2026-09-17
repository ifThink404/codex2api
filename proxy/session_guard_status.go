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

type SessionGuardStatus struct {
	StartedAt      string                      `json:"started_at"`
	Settings       SessionGuardSettingsStatus  `json:"settings"`
	TurnState      SessionGuardTurnStateStatus `json:"turn_state"`
	Borrow         auth.SessionBorrowStats     `json:"borrow"`
	InitialSession SessionGuardInitialStatus   `json:"initial_session"`
	AutoLock       SessionGuardAutoLockStatus  `json:"auto_lock"`
}

// SessionGuardStatusSnapshot 供 /api/admin/runtime 使用：全部是进程内计数，重启清零。
func SessionGuardStatusSnapshot(store *auth.Store) SessionGuardStatus {
	settings := CurrentRuntimeSettings()
	totals, accounts := sessionGuardTurnStateSnapshot()
	if accounts == nil {
		accounts = []SessionGuardTurnStateAccount{}
	}
	recent, since := sessionGuardInitialSnapshot(time.Now())
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
	}
	if store != nil {
		status.Settings.NoBorrowEnabled = store.SessionNoBorrowEnabled()
		status.Settings.NoBorrowHoldSeconds = int(store.SessionNoBorrowHold() / time.Second)
		status.Borrow = store.SessionBorrowStats()
	}
	return status
}

// SessionGuardStatusSnapshotForHandler 在 SessionGuardStatusSnapshot 之上补上自动锁定
// 计数（需要 *Handler 才能读 DB 预热锁表），供拿到 Handler 的调用方使用。
func SessionGuardStatusSnapshotForHandler(h *Handler) SessionGuardStatus {
	var store *auth.Store
	if h != nil {
		store = h.store
	}
	status := SessionGuardStatusSnapshot(store)
	status.AutoLock = sessionAutoLockSnapshot(h)
	return status
}
