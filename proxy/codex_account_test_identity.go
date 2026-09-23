package proxy

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/codex2api/auth"
)

func codexAccountTestScope(account *auth.Account) string {
	return codexIdentityDigest("codex-account-test-v1", strconv.FormatInt(account.ID(), 10), account.EffectiveAccountID())
}

// ResolveCodexAccountTestSessionID persists one test root per account, effective
// workspace and local calendar day. time.Now uses the server's configured TZ;
// midnight affects the next probe, never an already constructed request.
func ResolveCodexAccountTestSessionID(ctx context.Context, store CodexIdentityStore, account *auth.Account) (string, error) {
	return resolveCodexAccountTestSessionIDAt(ctx, store, account, time.Now())
}

func resolveCodexAccountTestSessionIDAt(ctx context.Context, store CodexIdentityStore, account *auth.Account, now time.Time) (string, error) {
	if account == nil || account.ID() <= 0 {
		return "", fmt.Errorf("测试账号缺少持久化 ID")
	}
	scope := codexIdentityDigest("codex-account-test-daily-v1", codexAccountTestScope(account), now.Format(time.DateOnly))
	if store == nil {
		// Database-free embedded users also keep a stable root within the day.
		return DeriveStableSessionUUIDv7(scope), nil
	}
	key := codexIdentityDigest("codex-account-test-root-key-v1", scope)
	entropy := codexIdentityDigest("codex-account-test-root-entropy-v1", scope)
	session, err := store.ResolveCodexIdentityUUIDv7(ctx, key, entropy)
	if err != nil {
		return "", fmt.Errorf("无法读取或保存账号测试主会话: %w", err)
	}
	return session, nil
}

// WithCodexAccountTestIdentityStore is reserved for administrator tests. Reusing
// a root requires a stable internal owner; anonymous user requests keep their
// existing per-request owners and cannot adopt this namespace.
func WithCodexAccountTestIdentityStore(ctx context.Context, store CodexIdentityStore, account *auth.Account) context.Context {
	if account != nil && account.ID() > 0 {
		// Keep ownership stable across midnight so in-flight probes can finish
		// using the root that was selected when their payload was constructed.
		owner := "account-test:" + codexAccountTestScope(account)
		ctx = context.WithValue(ctx, codexAnonymousIdentityContextKey{}, owner)
		ctx = context.WithValue(ctx, transportOwnerContextKey{}, owner)
	}
	if store == nil {
		return ctx
	}
	return WithCodexIdentityStore(ctx, store)
}
