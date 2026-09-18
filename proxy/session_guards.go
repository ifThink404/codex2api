package proxy

import (
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
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
	resetPromptPolicyCountersForTest()
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
// 还有一条无条件规则：入站值带替身前缀又没换回真实 token 的，不看开关一律剥离。
// 账号把 session_guards_policy 设成 off 时只剩这条兜底（dropUnresolvedSubstituteOnly）。
// headers 原地修改（调用方传的是本次尝试的下游头副本），body 返回可能改写后的副本。
//
// c 只用来把判定写进请求上下文供用量日志取用（noteUsageTurnStateEcho，首个判定优先），
// 不参与任何分类或剥离决策；为 nil 时（单测/内部调用）行为完全不变。
func (h *Handler) applyCodexTurnStateEchoPolicy(c *gin.Context, affinityKey string, account *auth.Account, headers http.Header, body []byte) ([]byte, turnStateEchoClass, bool) {
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
		// 客户端没回带（或这条请求没有会话标识可归属）：这也是一条事实，
		// 记 none 与「未记录」（'' ）区分开。
		noteUsageTurnStateEcho(c, usageTurnStateEchoNone, false)
		return body, turnStateEchoNone, false
	}
	// session_guards_policy=off：该账号的请求不分类、不剥离真实 token、不计分类计数，
	// 只保留「网关自造的替身不出网关」这条兜底。
	if account != nil && account.SessionGuardsOff() {
		return h.dropUnresolvedSubstituteOnly(affinityKey, account, headers, body)
	}
	class := turnStateEchoUnknown
	restored := ""
	// 是否托管必须和下发侧同一判据（官方 Codex 账号才托管），否则 relay 账号会被按
	// 替身语义剥离；relay 走 else 分支，保持第一轮的溯源分类。
	vaultApplies := turnStateVaultAppliesTo(account)
	if vaultApplies {
		// 托管开启：只有本会话当前替身能换回真实值；其余（外来真实 token、旧替身）一律剥离。
		restored, class = resolveCodexTurnStateSubstitute(affinityKey, account, token)
		if class == turnStateEchoUnknown {
			turnStateVaultForeign.Add(1)
		}
	} else {
		class = h.classifyCodexTurnStateEcho(affinityKey, account)
	}
	// 没能换回真实值的替身绝不能出网关：`c2a-ts-v1.` 是网关自造的、唯一且可稳定识别的
	// 标记，交给上游等于把「这个客户端在走 codex2api」直接送进风控管线——正是托管要
	// 消除的那类信号。托管关闭（运维对比开关）、relay 账号、上一轮的死替身都会落到这里，
	// 因此这条与 vaultApplies / strict 无关，一律剥离。
	unresolvedSubstitute := restored == "" && (IsCodexTurnStateSubstitute(token) || IsCodexTurnStateSubstitute(bodyToken))
	if unresolvedSubstitute {
		// cross（替身属于别的账号）已经是最准确的归类，保留；其余一律记为 unknown。
		if class != turnStateEchoCross {
			class = turnStateEchoUnknown
		}
		// 托管路径上的 unknown 已经在上面计过数，这里只补托管不生效时的那部分。
		if !vaultApplies {
			turnStateVaultForeign.Add(1)
		}
	}
	strip := unresolvedSubstitute || class == turnStateEchoCross || (class == turnStateEchoUnknown && (vaultApplies || CurrentRuntimeSettings().CodexTurnStateStrict))
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
	// 用量日志把「本会话替身已换回真实值」单独归一类：那是网关自己发出去的东西被
	// 原样带回来，与客户端拿着别的账号的真实 token（cross）完全不是一回事，合并成
	// same 会让按账号读数的页面看不出托管是否在正常续链。
	echo := string(class)
	if restored != "" {
		echo = usageTurnStateEchoSubstitute
	}
	noteUsageTurnStateEcho(c, echo, stripped)
	if class != turnStateEchoSame {
		log.Printf("[TURN-STATE] account=%d class=%s stripped=%t affinity=%s", accountID, class, stripped, hashRiskIdentity(affinityKey))
	}
	return body, class, stripped
}

// projectCodexTurnStateForWebsocket 严格模式下把仍留在出站头里的 token 挪进帧体
// （官方 WS v2 契约：token 走 response.create.client_metadata，握手头逐连接冻结、
// 不能承载逐轮状态）。帧体已有 token 时以帧体为准。legacy 模式原样返回。
//
// 替身例外：网关自造的 `c2a-ts-v1.…` 只删不投。strict 打开时这里是它绕开握手头那道闸
// 的唯一通路——/v1/chat/completions 与 /v1/messages 都不经 applyCodexTurnStateEchoPolicy，
// 投进 client_metadata 就等于把网关的唯一标记送进上游帧体。
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
	if IsCodexTurnStateSubstitute(token) {
		// 头已经从副本里删掉，帧体原样返回：整枚丢弃，不投也不带。
		NoteCodexTurnStateSubstituteDropped()
		log.Printf("[TURN-STATE] substitute dropped before websocket projection")
		return body, out
	}
	if gjson.GetBytes(body, codexTurnStateBodyPath).Exists() || !gjson.ValidBytes(body) {
		return body, out
	}
	updated, err := sjson.SetBytes(body, codexTurnStateBodyPath, token)
	if err != nil {
		return body, out
	}
	return updated, out
}

// dropCodexTurnStateSubstituteFromBody 把帧体里网关自造的替身摘掉，返回改写后的 body
// 和是否摘过。给的是中转（relay）分支：它在 applyCodexTurnStateEchoPolicy 之前就返回，
// 转发的又是 PrepareOpenAIResponsesBody 原样保留的客户端 body（未知字段照抄，
// client_metadata 会活下来），所以帧体位置的回带在那条路上没有别的闸。
// 真实 token 不动——中转保持第一轮的透传语义。
func dropCodexTurnStateSubstituteFromBody(body []byte) ([]byte, bool) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body, false
	}
	if token := strings.TrimSpace(gjson.GetBytes(body, codexTurnStateBodyPath).String()); token == "" || !IsCodexTurnStateSubstitute(token) {
		return body, false
	}
	updated, err := sjson.DeleteBytes(body, codexTurnStateBodyPath)
	if err != nil {
		return body, false
	}
	NoteCodexTurnStateSubstituteDropped()
	log.Printf("[TURN-STATE] substitute dropped from the outbound request body")
	return updated, true
}
