package proxy

import (
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 会话防护：把「别的账号铸造的 turn-state 被回带到当前账号」这类跨账号信号
// 分类、计数并按策略剥离。分类只回答一个问题——这个 token 是谁铸造的：
//   same    溯源账号 == 本次选中的账号
//   cross   溯源账号 != 本次账号（真实 Codex 永远不会产生，一律剥离）
//   unknown 没有溯源（绑定过期/重启/另一实例/从未记录）；legacy 透传，strict 剥离
// 溯源顺序：HTTP 下发时记录的精确来源表 → 亲和键当前绑定账号（含跨进程缓存）→ unknown。

type turnStateEchoClass string

const (
	turnStateEchoNone    turnStateEchoClass = "none"
	turnStateEchoSame    turnStateEchoClass = "same"
	turnStateEchoCross   turnStateEchoClass = "cross"
	turnStateEchoUnknown turnStateEchoClass = "unknown"
)

const codexTurnStateBodyPath = "client_metadata.x-codex-turn-state"

type SessionGuardTurnStateCounters struct {
	Same     uint64 `json:"same"`
	Cross    uint64 `json:"cross"`
	Unknown  uint64 `json:"unknown"`
	Stripped uint64 `json:"stripped"`
}

type SessionGuardTurnStateAccount struct {
	AccountID int64                         `json:"account_id"`
	Counters  SessionGuardTurnStateCounters `json:"counters"`
}

type sessionGuardStatsState struct {
	mu       sync.Mutex
	started  time.Time
	totals   SessionGuardTurnStateCounters
	accounts map[int64]*SessionGuardTurnStateCounters
}

var sessionGuardStats = newSessionGuardStats()

func newSessionGuardStats() *sessionGuardStatsState {
	return &sessionGuardStatsState{started: time.Now().UTC(), accounts: make(map[int64]*SessionGuardTurnStateCounters)}
}

func resetSessionGuardStatsForTest() {
	fresh := newSessionGuardStats()
	sessionGuardStats.mu.Lock()
	sessionGuardStats.started = fresh.started
	sessionGuardStats.totals = SessionGuardTurnStateCounters{}
	sessionGuardStats.accounts = fresh.accounts
	sessionGuardStats.mu.Unlock()
	resetInitialSessionStatsForTest()
}

func (c *SessionGuardTurnStateCounters) add(class turnStateEchoClass, stripped bool) {
	switch class {
	case turnStateEchoSame:
		c.Same++
	case turnStateEchoCross:
		c.Cross++
	case turnStateEchoUnknown:
		c.Unknown++
	default:
		return
	}
	if stripped {
		c.Stripped++
	}
}

func recordTurnStateObservation(accountID int64, class turnStateEchoClass, stripped bool) {
	if class == turnStateEchoNone {
		return
	}
	s := sessionGuardStats
	s.mu.Lock()
	defer s.mu.Unlock()
	s.totals.add(class, stripped)
	if accountID > 0 {
		counters := s.accounts[accountID]
		if counters == nil {
			// 账号数有上界（账号池），map 不会无界增长。
			counters = &SessionGuardTurnStateCounters{}
			s.accounts[accountID] = counters
		}
		counters.add(class, stripped)
	}
}

func sessionGuardTurnStateSnapshot() (SessionGuardTurnStateCounters, []SessionGuardTurnStateAccount) {
	s := sessionGuardStats
	s.mu.Lock()
	defer s.mu.Unlock()
	accounts := make([]SessionGuardTurnStateAccount, 0, len(s.accounts))
	for id, counters := range s.accounts {
		accounts = append(accounts, SessionGuardTurnStateAccount{AccountID: id, Counters: *counters})
	}
	sort.Slice(accounts, func(i, j int) bool {
		li := accounts[i].Counters.Cross + accounts[i].Counters.Unknown
		lj := accounts[j].Counters.Cross + accounts[j].Counters.Unknown
		if li != lj {
			return li > lj
		}
		return accounts[i].AccountID < accounts[j].AccountID
	})
	if len(accounts) > 20 {
		accounts = accounts[:20]
	}
	return s.totals, accounts
}

func sessionGuardStartedAt() time.Time {
	s := sessionGuardStats
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

// classifyCodexTurnStateEcho 只分类，不改任何东西。
func (h *Handler) classifyCodexTurnStateEcho(affinityKey string, account *auth.Account) turnStateEchoClass {
	if account == nil || account.ID() <= 0 {
		return turnStateEchoUnknown
	}
	if raw, ok := codexTurnStateOrigins.Load(affinityKey); ok {
		if origin, ok := raw.(codexTurnStateOrigin); ok && (origin.expiresAt.IsZero() || time.Now().Before(origin.expiresAt)) {
			if origin.accountID == account.ID() {
				return turnStateEchoSame
			}
			return turnStateEchoCross
		}
	}
	if h != nil && h.store != nil {
		if bound, ok := h.store.SessionAffinityAccountID(affinityKey); ok && bound > 0 {
			if bound == account.ID() {
				return turnStateEchoSame
			}
			return turnStateEchoCross
		}
	}
	return turnStateEchoUnknown
}

// applyCodexTurnStateEchoPolicy 在选号之后、出站之前调用一次（HTTP 与下游 WS 两条
// 尝试循环都调）。cross 一律剥离头 + 体；unknown 仅 strict 剥离；same/none 不动。
// headers 原地修改（调用方传的是本次尝试的下游头副本），body 返回可能改写后的副本。
func (h *Handler) applyCodexTurnStateEchoPolicy(affinityKey string, account *auth.Account, headers http.Header, body []byte) ([]byte, turnStateEchoClass, bool) {
	affinityKey = strings.TrimSpace(affinityKey)
	token := ""
	if headers != nil {
		token = strings.TrimSpace(headers.Get(codexTurnStateHeader))
	}
	bodyToken := strings.TrimSpace(gjson.GetBytes(body, codexTurnStateBodyPath).String())
	if token == "" {
		token = bodyToken
	}
	if token == "" || affinityKey == "" {
		return body, turnStateEchoNone, false
	}
	class := turnStateEchoUnknown
	restored := ""
	if turnStateVaultEnabled() {
		// 托管开启：只有本会话当前替身能换回真实值；其余（外来真实 token、旧替身）一律剥离。
		restored, class = resolveCodexTurnStateSubstitute(affinityKey, account, token)
		if class == turnStateEchoUnknown {
			turnStateVaultForeign.Add(1)
		}
	} else {
		class = h.classifyCodexTurnStateEcho(affinityKey, account)
	}
	strip := class == turnStateEchoCross || (class == turnStateEchoUnknown && (turnStateVaultEnabled() || CurrentRuntimeSettings().CodexTurnStateStrict))
	if restored != "" && class == turnStateEchoSame {
		if headers != nil && headers.Get(codexTurnStateHeader) != "" {
			headers.Set(codexTurnStateHeader, restored)
		}
		if bodyToken != "" {
			if updated, err := sjson.SetBytes(body, codexTurnStateBodyPath, restored); err == nil {
				body = updated
			}
		}
		turnStateVaultRestore.Add(1)
	}
	stripped := false
	if strip {
		if headers != nil && headers.Get(codexTurnStateHeader) != "" {
			headers.Del(codexTurnStateHeader)
			stripped = true
		}
		if bodyToken != "" {
			if updated, err := sjson.DeleteBytes(body, codexTurnStateBodyPath); err == nil {
				body = updated
				stripped = true
			}
		}
	}
	accountID := int64(0)
	if account != nil {
		accountID = account.ID()
	}
	recordTurnStateObservation(accountID, class, stripped)
	if class != turnStateEchoSame {
		log.Printf("[TURN-STATE] account=%d class=%s stripped=%t affinity=%s", accountID, class, stripped, hashRiskIdentity(affinityKey))
	}
	return body, class, stripped
}

// projectCodexTurnStateForWebsocket 严格模式下把仍留在出站头里的 token 挪进帧体
// （官方 WS v2 契约：token 走 response.create.client_metadata，握手头逐连接冻结、
// 不能承载逐轮状态）。帧体已有 token 时以帧体为准。legacy 模式原样返回。
func projectCodexTurnStateForWebsocket(body []byte, headers http.Header) ([]byte, http.Header) {
	if !CurrentRuntimeSettings().CodexTurnStateStrict || headers == nil {
		return body, headers
	}
	token := strings.TrimSpace(headers.Get(codexTurnStateHeader))
	if token == "" {
		return body, headers
	}
	out := headers.Clone()
	out.Del(codexTurnStateHeader)
	if gjson.GetBytes(body, codexTurnStateBodyPath).Exists() || !gjson.ValidBytes(body) {
		return body, out
	}
	updated, err := sjson.SetBytes(body, codexTurnStateBodyPath, token)
	if err != nil {
		return body, out
	}
	return updated, out
}
