package proxy

import (
	"context"
	"net/http"
	"strings"

	"github.com/codex2api/auth"
)

type bpsFullConvergenceKey struct{}

type bpsFullConvergenceScope struct {
	taskKey   string
	turnScope string
}

func bpsFullConvergenceFrom(ctx context.Context) *bpsFullConvergenceScope {
	if ctx == nil {
		return nil
	}
	scope, _ := ctx.Value(bpsFullConvergenceKey{}).(*bpsFullConvergenceScope)
	return scope
}

// Only the BPS task is shared. Keep the original conversation partition in the
// turn key and leave routing, attachments, response caches and ownership alone.
func withBPSFullConvergence(ctx context.Context, account *auth.Account, profile bpsProfileConfig, headers http.Header, fingerprint *CodexFingerprint, cacheKey, apiKey string) (context.Context, error) {
	if account.EffectiveCodexFingerprintMode() != auth.CodexFingerprintModeFull {
		if bpsFullConvergenceFrom(ctx) != nil {
			ctx = context.WithValue(ctx, bpsFullConvergenceKey{}, (*bpsFullConvergenceScope)(nil))
		}
		return ctx, nil
	}
	upstreamAccount := strings.TrimSpace(account.EffectiveAccountID())
	if upstreamAccount == "" {
		return ctx, codexAccountIdentityError("BPS 全部收敛需要有效的上游账号 ID。")
	}
	owner := verifiedTransportUser(ctx)
	if owner == "" {
		owner = codexIdentityDigest("bps-full-api-key", apiKey)
	}
	// The snapshot retains separate source conversations even in legacy/full
	// mode, where applying the body fingerprint folds session/thread fields.
	if fingerprint != nil {
		headers = fingerprint.DownstreamHeaders()
	}
	session, thread := extractClientCodexIdentity(headers)
	if session == "" && thread == "" {
		session = cacheKey
	}
	scope := &bpsFullConvergenceScope{
		taskKey: codexIdentityDigest("bps-full-account-task-v1", upstreamAccount),
		turnScope: codexIdentityDigest("bps-full-private-turn-v1", upstreamAccount, owner,
			string(profile.profile), session, thread, outboundEpochFromContext(ctx).identityKey()),
	}
	return context.WithValue(ctx, bpsFullConvergenceKey{}, scope), nil
}
