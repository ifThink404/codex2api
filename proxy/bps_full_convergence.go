package proxy

import (
	"context"
	"net/http"
	"strings"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

type bpsFullConvergenceKey struct{}

type bpsTurnQuestionInputKey struct{}
type bpsTurnQuestionInput struct {
	session, thread, seed string
}

// Capture question identity before restart cleanup and account UUID rewriting.
// No message body or raw identity is persisted by the question counter.
func withBPSTurnQuestionInput(ctx context.Context, account *auth.Account, headers http.Header, body []byte) context.Context {
	if account == nil || account.EffectiveCodexFingerprintMode() != auth.CodexFingerprintModeTurnRound {
		return ctx
	}
	metadata := CodexRequestMetadataHeaders(headers, NormalizeCodexRequestMetadata(body))
	session, thread := extractClientCodexIdentity(metadata)
	seed := strings.TrimSpace(gjson.Get(metadata.Get(codexTurnMetadataHeader), "turn_id").String())
	if seed == "" {
		seed = strings.TrimSpace(gjson.GetBytes(body, "client_metadata.turn_id").String())
	}
	if seed == "" {
		seed, _, _ = bpsWordInputIdentity(body)
	}
	return context.WithValue(ctx, bpsTurnQuestionInputKey{}, &bpsTurnQuestionInput{session, thread, seed})
}

type bpsFullConvergenceScope struct {
	taskKey           string
	turnScope         string
	turnEpoch         string
	userScope         string
	questionScope     string
	questionTurnSeed  string
	roundLimit        int
	turnRoundLimit    int
	taskLifetimeHours int
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
	mode := account.EffectiveCodexFingerprintMode()
	if mode != auth.CodexFingerprintModeFull && mode != auth.CodexFingerprintModeRound && mode != auth.CodexFingerprintModeTurnRound {
		if bpsFullConvergenceFrom(ctx) != nil {
			ctx = context.WithValue(ctx, bpsFullConvergenceKey{}, (*bpsFullConvergenceScope)(nil))
		}
		return ctx, nil
	}
	upstreamAccount := strings.TrimSpace(account.EffectiveAccountID())
	if upstreamAccount == "" {
		return ctx, codexAccountIdentityError("BPS 账号级任务收敛需要有效的上游账号 ID。")
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
	turnEpoch := outboundEpochFromContext(ctx).identityKey()
	if mode == auth.CodexFingerprintModeRound || mode == auth.CodexFingerprintModeTurnRound {
		turnEpoch = bpsConvergenceTurnEpoch(ctx, account)
	}
	scope := &bpsFullConvergenceScope{
		taskKey:   codexIdentityDigest("bps-full-account-task-v1", upstreamAccount),
		turnEpoch: turnEpoch,
		turnScope: codexIdentityDigest("bps-full-private-turn-v1", upstreamAccount, owner,
			string(profile.profile), session, thread, turnEpoch),
	}
	if mode == auth.CodexFingerprintModeRound {
		// Start a fresh batch once when upgrading from per-step turns, so the
		// first fixed turn starts at iteration 1 rather than a partial counter.
		scope.taskKey = codexIdentityDigest("bps-round-account-task-v2", upstreamAccount)
		scope.roundLimit = CurrentRuntimeSettings().BPSRoundConvergenceLimit
	}
	if mode == auth.CodexFingerprintModeTurnRound {
		scope.taskKey = codexIdentityDigest("bps-turn-account-task-v1", upstreamAccount)
		scope.taskLifetimeHours = CurrentRuntimeSettings().BPSTurnTaskLifetimeHours
		scope.turnRoundLimit = CurrentRuntimeSettings().BPSTurnRoundLimit
		// Users share their question counter across windows. Include the original
		// conversation only in question deduplication, never in the user counter.
		scope.userScope = codexIdentityDigest("bps-turn-user-scope-v1", upstreamAccount, owner, string(profile.profile))
		scope.questionTurnSeed = strings.TrimSpace(gjson.Get(headers.Get(codexTurnMetadataHeader), "turn_id").String())
		if original, _ := ctx.Value(bpsTurnQuestionInputKey{}).(*bpsTurnQuestionInput); original != nil {
			if original.session != "" || original.thread != "" {
				session, thread = original.session, original.thread
			}
			if original.seed != "" {
				scope.questionTurnSeed = original.seed
			}
		}
		scope.questionScope = codexIdentityDigest("bps-turn-question-scope-v1", scope.userScope, session, thread)
	}
	return context.WithValue(ctx, bpsFullConvergenceKey{}, scope), nil
}
