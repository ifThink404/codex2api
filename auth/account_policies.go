package auth

import (
	"fmt"
	"strings"
)

// 账号级策略：三列都以 inherit 为默认——上线不改变任何行为，运维按账号逐个打开。
const (
	PolicyInherit            = "inherit"
	PromptFilterPolicyExempt = "exempt" // 命中 prompt 检测的请求落到该账号时放行
	EgressPolicyDirect       = "direct" // 不进代理池/全局代理/Resin，直连上游
	SessionGuardsPolicyOff   = "off"    // 不做 turn-state 分类剥离/托管、不计 500 连击、不做准入与不借用
)

// AccountPolicyPromptFilter 等三个常量是策略字段名；database 包不 import auth，
// 归一化只在 auth/admin 两层做，列名字符串在两边保持一致。
const (
	AccountPolicyPromptFilter  = "prompt_filter_policy"
	AccountPolicyEgress        = "egress_policy"
	AccountPolicySessionGuards = "session_guards_policy"
)

var accountPolicyValues = map[string][]string{
	AccountPolicyPromptFilter:  {PolicyInherit, PromptFilterPolicyExempt},
	AccountPolicyEgress:        {PolicyInherit, EgressPolicyDirect},
	AccountPolicySessionGuards: {PolicyInherit, SessionGuardsPolicyOff},
}

// NormalizeAccountPolicy 把库里/请求里的值收敛成该字段允许的枚举；空、未知、不属于该字段的值一律 inherit。
func NormalizeAccountPolicy(field, value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, allowed := range accountPolicyValues[field] {
		if value == allowed {
			return allowed
		}
	}
	return PolicyInherit
}

// ValidateAccountPolicy 供 admin PATCH 解析用：非法值直接 400，而不是静默归 inherit。
func ValidateAccountPolicy(field string) func(string) error {
	return func(value string) error {
		value = strings.ToLower(strings.TrimSpace(value))
		for _, allowed := range accountPolicyValues[field] {
			if value == allowed {
				return nil
			}
		}
		return fmt.Errorf("%s must be one of %s", field, strings.Join(accountPolicyValues[field], "/"))
	}
}

func (a *Account) PromptFilterExempt() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.PromptFilterPolicy == PromptFilterPolicyExempt
}

func (a *Account) EgressDirect() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.EgressPolicy == EgressPolicyDirect
}

func (a *Account) SessionGuardsOff() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.SessionGuardsPolicy == SessionGuardsPolicyOff
}

// ApplyAccountPolicyPatch 热更新内存账号的策略字段（nil = 不改），与 ApplyAccountSchedulerOverridePatch 同一套锁纪律。
// 不调用 recomputeSchedulerLocked / invalidateRoutingSchedulers：三个策略都不参与评分与并发计算，
// 所有读取方（代理解析、会话防护闸门、prompt 放行）都直接读账号指针，下一次请求即生效。
func (s *Store) ApplyAccountPolicyPatch(dbID int64, promptFilter, egress, sessionGuards *string) bool {
	if s == nil {
		return false
	}
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}
	acc.mu.Lock()
	if promptFilter != nil {
		acc.PromptFilterPolicy = NormalizeAccountPolicy(AccountPolicyPromptFilter, *promptFilter)
	}
	if egress != nil {
		acc.EgressPolicy = NormalizeAccountPolicy(AccountPolicyEgress, *egress)
	}
	if sessionGuards != nil {
		acc.SessionGuardsPolicy = NormalizeAccountPolicy(AccountPolicySessionGuards, *sessionGuards)
	}
	acc.mu.Unlock()
	return true
}
