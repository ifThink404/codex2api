package proxy

import (
	"context"
	"strings"

	"github.com/gin-gonic/gin"
)

type upstreamSourceVisibilityKey struct{}

// Bind per request/WS turn, including false, so a reused context cannot retain
// an exemption after signed identity verification fails or changes.
func bindUpstreamSourceVisibility(c *gin.Context, policy verifiedNewAPIPolicyContext, verified bool) {
	visible := verified && policy.MetaVerified && policy.APIKeyID == requestAPIKeyID(c) &&
		policy.VerificationSecret != "" && strings.TrimSpace(policy.Identity.UserID) != "" &&
		policy.Meta.PreserveUpstreamSource
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), upstreamSourceVisibilityKey{}, visible))
}

// This exempts source-name/address rewriting only. Protocol identity and
// credential filtering, response aliases and caller-tool projection still run.
func preserveUpstreamSource(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	visible, _ := ctx.Value(upstreamSourceVisibilityKey{}).(bool)
	return visible
}
