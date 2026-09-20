package proxy

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

func turnStateCtxFromGin(c *gin.Context) context.Context {
	if c == nil || c.Request == nil {
		return context.Background()
	}
	return c.Request.Context()
}

type codexTurnStateAffinityContextKey struct{}

func WithCodexTurnStateAffinityKey(ctx context.Context, key string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, codexTurnStateAffinityContextKey{}, strings.TrimSpace(key))
}

func CodexTurnStateAffinityKeyFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(codexTurnStateAffinityContextKey{}).(string)
	return value
}

func guardCodexTurnStateEcho(affinityKey string, account *auth.Account, headers http.Header) {
	guardCodexTurnStateEchoForAccount(affinityKey, account, headers)
}

func guardCodexTurnStateEchoForAccount(affinityKey string, account *auth.Account, headers http.Header) {
	if headers == nil {
		return
	}
	value := strings.TrimSpace(headers.Get(codexTurnStateHeader))
	foreign := IsCodexTurnStateSubstitute(value)
	if !foreign && strings.TrimSpace(affinityKey) != "" && account != nil {
		if raw, ok := codexTurnStateOrigins.Load(strings.TrimSpace(affinityKey)); ok {
			if origin, ok := raw.(codexTurnStateOrigin); ok && origin.accountID != account.ID() && time.Now().Before(origin.expiresAt) {
				foreign = true
			}
		}
	}
	if foreign {
		headers.Del(codexTurnStateHeader)
		NoteCodexTurnStateSubstituteDropped()
	}
}

func GuardCodexTurnStateEcho(affinityKey string, account *auth.Account, headers http.Header) {
	guardCodexTurnStateEchoForAccount(affinityKey, account, headers)
}
