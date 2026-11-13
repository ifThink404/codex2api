package auth

import (
	"fmt"
	"strings"
)

// 账号级策略：以 inherit 为默认——上线不改变任何行为，运维按账号逐个打开。
const (
	PolicyInherit      = "inherit"
	EgressPolicyDirect = "direct" // 不进代理池/全局代理/Resin，直连上游
)

// AccountPolicyEgress 是策略字段名；database 包不 import auth，
// 归一化只在 auth/admin 两层做，列名字符串在两边保持一致。
const AccountPolicyEgress = "egress_policy"

var accountPolicyValues = map[string][]string{
	AccountPolicyEgress: {PolicyInherit, EgressPolicyDirect},
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

func (a *Account) EgressDirect() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.EgressPolicy == EgressPolicyDirect
}

// ApplyAccountPolicyPatch 热更新内存账号的策略字段（nil = 不改），与 ApplyAccountSchedulerOverridePatch 同一套锁纪律。
// 不调用 recomputeSchedulerLocked / invalidateRoutingSchedulers：出网策略不参与评分与并发计算，
// 读取方（代理解析）直接读账号指针，下一次请求即生效。
func (s *Store) ApplyAccountPolicyPatch(dbID int64, egress *string) bool {
	if s == nil {
		return false
	}
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}
	acc.mu.Lock()
	if egress != nil {
		acc.EgressPolicy = NormalizeAccountPolicy(AccountPolicyEgress, *egress)
	}
	acc.mu.Unlock()
	return true
}
