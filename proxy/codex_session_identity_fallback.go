package proxy

import (
	"context"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type codexSessionIdentityFallbackKey struct{}

// This is outbound metadata only. It does not create routing, ownership or
// window-accounting authority. Those decisions use the original request.
type codexSessionIdentityFallback struct {
	root, thread, source string
	conflict             bool
}

func bindCodexSessionIdentityFallback(c *gin.Context, identity requestSessionIdentity, root requestRootSessionIdentity, policy verifiedNewAPIPolicyContext, verified bool) {
	fallback := codexSessionIdentityFallback{conflict: root.conflict}
	if !root.conflict && root.stable {
		if verified {
			// RootSessionID was checked against the signed fingerprint by root
			// resolution. Keep its mapping compatible with metadata-rich requests.
			if policy.Meta.RootSessionState == newAPIPolicyRootSessionResolved {
				fallback.root = strings.TrimSpace(policy.Meta.RootSessionID)
			}
			if fallback.root == "" {
				fallback.root = DeriveStableSessionUUIDv7(codexIdentityDigest("codex-signed-root-v1", verifiedTransportUser(c.Request.Context()), root.sessionID))
			}
			fallback.source = "signed_newapi"
		} else if root.nativeRoot {
			fallback.root, fallback.source = root.sessionID, "native_root"
		}
		fallback.thread = fallback.root
		if root.related && fallback.root != "" {
			leaf := ""
			if verified {
				leaf = strings.TrimSpace(policy.Meta.SessionFingerprint)
			}
			if leaf == "" {
				leaf = identity.relatedRequestID
			}
			// Missing leaf metadata must not merge different child executions
			// into the parent. The request identity is stable across its retries.
			if leaf != "" {
				fallback.thread = DeriveStableSessionUUIDv7(codexIdentityDigest("codex-fallback-leaf-v1", fallback.root, leaf))
			}
		}
	}
	// Replace the value on every resolution, including WS frames, so a stale
	// connection's signed identity cannot leak into a new request snapshot.
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), codexSessionIdentityFallbackKey{}, fallback))
}

func (fingerprint *CodexFingerprint) completeRelaxedSessionIdentity(ctx context.Context) {
	if ctx == nil || !CurrentRuntimeSettings().CodexForkAccountFallbackEnabled || !fingerprint.accountIdentityRequested {
		return
	}
	if strings.TrimSpace(fingerprint.headers.Get(codexSessionIDHeader)) != "" || strings.TrimSpace(fingerprint.headers.Get(codexLegacySessionIDHeader)) != "" {
		return
	}
	fallback, _ := ctx.Value(codexSessionIdentityFallbackKey{}).(codexSessionIdentityFallback)
	if fallback.conflict {
		return
	}
	thread := strings.TrimSpace(fingerprint.headers.Get(codexThreadIDHeader))
	if fallback.root == "" && thread != "" {
		fallback.root, fallback.source = thread, "thread_id"
	}
	if fallback.root == "" {
		if epoch := outboundEpochFromContext(ctx); epoch != nil && epoch.key != "" {
			// The already-authorized continuity key survives preflight, account
			// migration and later requests. Account/generation isolation is still
			// applied by the ordinary persistent identity mapper.
			fallback.root = DeriveStableSessionUUIDv7(codexIdentityDigest("codex-continuity-root-v1", epoch.key))
			fallback.source = "session_continuity"
		}
	}
	if fallback.root == "" {
		return
	}
	if thread != "" {
		fallback.thread = thread
	} else if fallback.thread == "" {
		fallback.thread = fallback.root
	}
	fingerprint.headers.Set(codexSessionIDHeader, fallback.root)
	fingerprint.headers.Set(codexThreadIDHeader, fallback.thread)
	fingerprint.sessionIdentityFallback = &fallback
}

func (fallback *codexSessionIdentityFallback) completeBody(body []byte) []byte {
	if fallback == nil {
		return body
	}
	metadata := gjson.GetBytes(body, "client_metadata")
	if metadata.Exists() && !metadata.IsObject() {
		return body
	}
	for _, field := range []struct{ name, value string }{{"session_id", fallback.root}, {"thread_id", fallback.thread}} {
		value := metadata.Get(field.name)
		if !value.Exists() || value.Type == gjson.Null || value.Type == gjson.String && strings.TrimSpace(value.String()) == "" {
			body, _ = sjson.SetBytes(body, "client_metadata."+field.name, field.value)
		}
	}
	embedded := metadata.Get("x-codex-turn-metadata")
	canonical := diagnosticMetadataObject(embedded)
	if canonical.IsObject() {
		raw := canonical.Raw
		for _, field := range []struct{ name, value string }{{"session_id", fallback.root}, {"thread_id", fallback.thread}} {
			value := canonical.Get(field.name)
			if !value.Exists() || value.Type == gjson.Null || value.Type == gjson.String && strings.TrimSpace(value.String()) == "" {
				raw, _ = sjson.Set(raw, field.name, field.value)
			}
		}
		if embedded.Type == gjson.String {
			body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata", raw)
		} else {
			body, _ = sjson.SetRawBytes(body, "client_metadata.x-codex-turn-metadata", []byte(raw))
		}
	}
	return body
}
