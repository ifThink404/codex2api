package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// turn-state 托管：真实的 X-Codex-Turn-State 只留在网关。上游铸造时按亲和键存下
// 真实值和铸造账号，给客户端一个随机替身；客户端回带替身，出站前换回真实值。
// 客户端拿不到真实 token，就无法带去别的账号/网关再带回来污染铸造账号。
// 官方契约：token 每轮一枚、本轮内原样回带（openai/codex codex-rs client.rs）。

const (
	codexTurnStateSubstitutePrefix = "c2a-ts-v1."
	turnStateVaultTTL              = time.Hour
	turnStateVaultSweepEvery       = 256
)

type turnStateVaultEntry struct {
	real       string
	substitute string
	accountID  int64
	expiresAt  time.Time
}

type SessionGuardVaultCounters struct {
	Issued          uint64 `json:"issued"`
	Restored        uint64 `json:"restored"`
	ForeignStripped uint64 `json:"foreign_stripped"`
}

var (
	turnStateVault        sync.Map // affinityKey -> *turnStateVaultEntry
	turnStateVaultWrites  atomic.Uint64
	turnStateVaultIssued  atomic.Uint64
	turnStateVaultRestore atomic.Uint64
	turnStateVaultForeign atomic.Uint64
)

func resetTurnStateVaultForTest() {
	turnStateVault.Range(func(k, _ any) bool { turnStateVault.Delete(k); return true })
	turnStateVaultIssued.Store(0)
	turnStateVaultRestore.Store(0)
	turnStateVaultForeign.Store(0)
}

func turnStateVaultCountersSnapshot() SessionGuardVaultCounters {
	return SessionGuardVaultCounters{Issued: turnStateVaultIssued.Load(), Restored: turnStateVaultRestore.Load(), ForeignStripped: turnStateVaultForeign.Load()}
}

func turnStateVaultEnabled() bool { return CurrentRuntimeSettings().CodexTurnStateVaultEnabled }

// turnStateVaultAppliesTo 托管只作用于官方 Codex 账号；relay/Grok/Antigravity/Claude
// 账号的 token 原样透传（第一轮语义）。这些账号走 Responses 的 relay 分支，出站不经
// applyCodexTurnStateEchoPolicy，替身没人换得回去——下一轮上游会收到网关自造的值，
// 续链直接断掉。
func turnStateVaultAppliesTo(account *auth.Account) bool {
	return turnStateVaultEnabled() && account != nil && account.ID() > 0 && !account.IsRelayStyle()
}

// turnStateSubstituteGenerator 是替身生成的测试接缝（crypto/rand 失败无法在测试里触发）。
var turnStateSubstituteGenerator = newCodexTurnStateSubstitute

func newCodexTurnStateSubstitute() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return ""
	}
	return codexTurnStateSubstitutePrefix + hex.EncodeToString(raw[:])
}

// IsCodexTurnStateSubstitute 判断一个 turn-state 值是不是网关自己铸造的替身。
// 前缀只此一处：回带策略（applyCodexTurnStateEchoPolicy）、HTTP 出站白名单透传
// （applyCodexAllowedForwardHeaders）与上游 WS 握手头装配
// （wsrelay.Executor.prepareWebsocketHeaders）共用同一个判据——替身是网关独有的标记，
// 换不回真实 token 时无论走哪条路都不许出网关。wsrelay 导入 proxy（反向没有依赖），
// 所以判据与计数钩子从这里导出给它用。
func IsCodexTurnStateSubstitute(value string) bool {
	return strings.HasPrefix(strings.TrimSpace(value), codexTurnStateSubstitutePrefix)
}

// NoteCodexTurnStateSubstituteDropped 记一次「替身在出站前被拦下」，计入
// session_guards.turn_state.vault.foreign_stripped。所有拦截点都走这里，
// 包内包外只有这一个累加入口。
func NoteCodexTurnStateSubstituteDropped() {
	turnStateVaultForeign.Add(1)
}

// issueCodexTurnStateSubstitute 记录真实 token 并返回替身；托管不适用该账号
// （关闭 / relay 账号）或输入为空返回 ""，由调用方按失败关闭处理。
func issueCodexTurnStateSubstitute(affinityKey string, account *auth.Account, real string) string {
	affinityKey, real = strings.TrimSpace(affinityKey), strings.TrimSpace(real)
	if !turnStateVaultAppliesTo(account) || affinityKey == "" || real == "" {
		return ""
	}
	// 同一轮的真实 token 会经两个载体下发（HTTP 响应头 + response.metadata 事件的
	// headers 对象；WS 传输只有后者）。同值必须复用同一枚替身：否则后铸的会顶掉先铸的，
	// 客户端按官方契约回带先拿到的那枚就永远换不回真实值，托管退化成「一律剥离」。
	// 换了真实 token（下一轮）才铸新替身，旧替身随即失效。
	if raw, ok := turnStateVault.Load(affinityKey); ok {
		if entry, ok := raw.(*turnStateVaultEntry); ok &&
			entry.real == real && entry.accountID == account.ID() && time.Now().Before(entry.expiresAt) {
			return entry.substitute
		}
	}
	substitute := turnStateSubstituteGenerator()
	if substitute == "" {
		return ""
	}
	turnStateVault.Store(affinityKey, &turnStateVaultEntry{real: real, substitute: substitute, accountID: account.ID(), expiresAt: time.Now().Add(turnStateVaultTTL)})
	turnStateVaultIssued.Add(1)
	if turnStateVaultWrites.Add(1)%turnStateVaultSweepEvery == 0 {
		now := time.Now()
		turnStateVault.Range(func(k, v any) bool {
			if entry, ok := v.(*turnStateVaultEntry); ok && now.After(entry.expiresAt) {
				turnStateVault.Delete(k)
			}
			return true
		})
	}
	return substitute
}

// resolveCodexTurnStateSubstitute 把回带的替身换回真实值：same 返回真实 token；
// cross（替身属于别的账号）与 unknown（不是本会话当前替身/已过期）返回空。
func resolveCodexTurnStateSubstitute(affinityKey string, account *auth.Account, inbound string) (string, turnStateEchoClass) {
	inbound = strings.TrimSpace(inbound)
	raw, ok := turnStateVault.Load(strings.TrimSpace(affinityKey))
	if !ok {
		return "", turnStateEchoUnknown
	}
	entry, ok := raw.(*turnStateVaultEntry)
	if !ok || time.Now().After(entry.expiresAt) || inbound == "" || inbound != entry.substitute {
		return "", turnStateEchoUnknown
	}
	if account == nil || account.ID() != entry.accountID {
		return "", turnStateEchoCross
	}
	return entry.real, turnStateEchoSame
}

// vaultCodexTurnStateEvent 改写 response.metadata / codex.response.metadata 事件里
// headers 对象的 x-codex-turn-state（官方客户端从这里读 token），其余事件原样返回。
func (h *Handler) vaultCodexTurnStateEvent(affinityKey string, account *auth.Account, eventType string, data []byte) []byte {
	if !turnStateVaultAppliesTo(account) {
		return data
	}
	switch strings.TrimSpace(eventType) {
	case "response.metadata", "codex.response.metadata":
	default:
		return data
	}
	if len(data) == 0 || !gjson.ValidBytes(data) {
		return data
	}
	headers := gjson.GetBytes(data, "headers")
	if !headers.IsObject() {
		return data
	}
	name, token := "", ""
	headers.ForEach(func(k, v gjson.Result) bool {
		if strings.EqualFold(k.String(), "x-codex-turn-state") && v.Type == gjson.String && strings.TrimSpace(v.String()) != "" {
			name, token = k.String(), strings.TrimSpace(v.String())
			return false
		}
		return true
	})
	if token == "" {
		return data
	}
	substitute := issueCodexTurnStateSubstitute(affinityKey, account, token)
	if substitute == "" {
		// 失败关闭：托管该管这个账号却没铸出替身时，宁可把字段删掉，也不能把真实
		// token 留在给客户端的事件里。无会话标识同样丢弃，但那不是故障，不记日志。
		if strings.TrimSpace(affinityKey) != "" {
			log.Printf("[TURN-STATE] vault issue failed, event field dropped account=%d affinity=%s", account.ID(), hashRiskIdentity(affinityKey))
		}
		if pruned, err := sjson.DeleteBytes(data, "headers."+name); err == nil {
			return pruned
		}
		return data
	}
	noteCodexTurnStateProvenance(affinityKey, account)
	updated, err := sjson.SetBytes(data, "headers."+name, substitute)
	if err != nil {
		return data
	}
	return updated
}
