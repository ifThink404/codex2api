package auth

import "testing"

// TestResolveProxyForAccountDirectPolicy 验证 egress_policy=direct 绕过代理池/
// 全局代理:不进池、不用全局代理,直连返回 ("", true)。固定 proxy_url 仍然优先——
// 这是运维显式绑了出口,不该被账号级 direct 策略吞掉。
func TestResolveProxyForAccountDirectPolicy(t *testing.T) {
	s := &Store{
		globalProxy:      "http://global-proxy:8080",
		proxyPoolEnabled: true,
		proxyPool:        []string{"socks5://pool-a:1080"},
	}
	acc := &Account{DBID: 1}
	acc.EgressPolicy = EgressPolicyDirect
	if got, direct := s.resolveProxyForAccountSnapshot(acc); got != "" || !direct {
		t.Fatalf("direct policy must bypass the pool: got %q direct=%v", got, direct)
	}

	acc.ProxyURL = "socks5://pinned:1080"
	if got, _ := s.resolveProxyForAccountSnapshot(acc); got != "socks5://pinned:1080" {
		t.Fatalf("pinned proxy keeps priority over direct: got %q", got)
	}

	acc.ProxyURL = ""
	acc.EgressPolicy = PolicyInherit
	if got, _ := s.resolveProxyForAccountSnapshot(acc); got != "socks5://pool-a:1080" {
		t.Fatalf("inherit must still use the pool: got %q", got)
	}
}
