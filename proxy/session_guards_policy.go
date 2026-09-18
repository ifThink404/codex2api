package proxy

import (
	"log"
	"net/http"
	"strings"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// sessionGuardsActiveFor：会话防护（turn-state 分类/托管、500 连击自动锁、首次会话
// 准入、不借用、窗口号记录）只作用于官方 Codex 账号，且账号没有把
// session_guards_policy 设成 off。
//
// 这几道闸共用一个判据是刻意的：它们守的是同一个假设——请求由官方 Codex 客户端
// 发出、会话与账号一一对应。relay/Grok/Antigravity/Claude 账号本来就不满足这个假设
// （IsRelayStyle）；policy=off 则是运营者对某个官方账号手动宣布「这号上的客户端不按
// Codex 契约走，别管它」。两者要一起放开，否则会出现「分类剥离关了但准入还在拒」
// 这种半开半关的状态。
//
// ID() <= 0 的账号（还没落库的测试/临时对象）一律算防护外：所有防护都要按账号归因，
// 没有 ID 就归不了因。
func sessionGuardsActiveFor(account *auth.Account) bool {
	return account != nil && account.ID() > 0 && !account.IsRelayStyle() && !account.SessionGuardsOff()
}

// dropUnresolvedSubstituteOnly 是 policy=off 账号走的那条最小路径：不分类、不记
// 分类计数、不碰真实 token，只保留一条无条件的兜底——网关自造的 `c2a-ts-v1.` 替身
// 绝不出网关。关掉防护是运营者对「别改我的请求」的要求，而替身根本不是客户端的东西，
// 是上一轮托管开着时网关发给它的；把它交给上游等于送一个「这个客户端在走 codex2api」
// 的唯一标记，任何开关都不该打开这道门。
//
// 返回值与 applyCodexTurnStateEchoPolicy 同形：改写后的 body、分类、是否剥离过。
// 分类一律 none（没分类过就不谎报 same/cross），剥离过的记一次 foreign_stripped。
func (h *Handler) dropUnresolvedSubstituteOnly(affinityKey string, account *auth.Account, headers http.Header, body []byte) ([]byte, turnStateEchoClass, bool) {
	headerToken := ""
	if headers != nil {
		headerToken = strings.TrimSpace(headers.Get(codexTurnStateHeader))
	}
	bodyToken := strings.TrimSpace(gjson.GetBytes(body, codexTurnStateBodyPath).String())
	if !IsCodexTurnStateSubstitute(headerToken) && !IsCodexTurnStateSubstitute(bodyToken) {
		return body, turnStateEchoNone, false
	}
	// 逐载体判断：两个载体理论上同值，但真出现「头是真实值、体是替身」这种混搭时，
	// 只摘替身那一枚——policy=off 的承诺是不动客户端的真实 token。
	stripped := false
	if headers != nil && IsCodexTurnStateSubstitute(headerToken) {
		headers.Del(codexTurnStateHeader)
		stripped = true
	}
	if IsCodexTurnStateSubstitute(bodyToken) {
		if updated, err := sjson.DeleteBytes(body, codexTurnStateBodyPath); err == nil {
			body = updated
			stripped = true
		}
	}
	if stripped {
		NoteCodexTurnStateSubstituteDropped()
		accountID := int64(0)
		if account != nil {
			accountID = account.ID()
		}
		log.Printf("[TURN-STATE] substitute dropped on a guards-off account account=%d affinity=%s", accountID, hashRiskIdentity(affinityKey))
	}
	return body, turnStateEchoNone, stripped
}
