package proxy

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const upstreamErrorDiagnosticHeader = "X-Codex2API-Error-Diagnostic"
const upstreamErrorDiagnosticDomain = "codex2api:error-diagnostic:v1"
const upstreamErrorDiagnosticKey = "codex2api_upstream_error_diagnostic"
const upstreamErrorDiagnosticSize = 2048

// HTTPStatus is the observed upstream response status, never the gateway's
// fallback 500 or the inferred status of an SSE error event.
type upstreamErrorDiagnostic struct {
	RequestID         string `json:"request_id"`
	ChannelID         int    `json:"channel_id"`
	IssuedAt          int64  `json:"issued_at"`
	Message           string `json:"message"`
	Code              string `json:"code,omitempty"`
	Type              string `json:"type,omitempty"`
	Source            string `json:"source"`
	Stage             string `json:"stage"`
	Transport         string `json:"transport,omitempty"`
	HTTPStatus        int    `json:"http_status,omitempty"`
	HandshakeStatus   int    `json:"handshake_status,omitempty"`
	UpstreamRequestID string `json:"upstream_request_id,omitempty"`
	GatewayRequestID  string `json:"gateway_request_id,omitempty"`
}

type upstreamErrorDiagnosticState struct {
	diagnostic upstreamErrorDiagnostic
	attempt    *upstreamTraceAttempt
	envelope   string
}

var diagnosticURL = regexp.MustCompile(`(?i)(?:https?|wss?|socks5h?)://[^\s<>"']+`)
var diagnosticHost = regexp.MustCompile(`(?i)\b(?:[a-z0-9_-]+\.)+[a-z][a-z0-9-]{1,62}\b`)
var diagnosticIP = regexp.MustCompile(`\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}\b|\[[0-9a-fA-F:]+\]`)
var diagnosticToken = regexp.MustCompile(`\b(?:eyJ[A-Za-z0-9_.-]+|[A-Za-z0-9_+/=-]{80,})`)

func upstreamErrorSafeMessage(c *gin.Context, value string) string {
	if c != nil && c.Request != nil {
		if audit := upstreamTraceFromContext(c.Request.Context()); audit != nil && audit.store != nil {
			snapshot := snapshotUpstreamTrace(c.Request.Context())
			if account := audit.store.FindByID(snapshot.accountID); account != nil {
				_, key := account.OpenAIResponsesCredentials()
				for _, secret := range []string{account.GetAccessToken(), key} {
					if secret != "" {
						value = strings.ReplaceAll(value, secret, "[REDACTED]")
					}
				}
			}
		}
	}
	if c != nil {
		value = serviceErrorSafeText(c, value, 8192)
	} else {
		value = security.MaskSensitiveData(value)
	}
	value = diagnosticURL.ReplaceAllString(value, "[upstream URL]")
	value = diagnosticHost.ReplaceAllStringFunc(value, func(host string) string {
		if strings.EqualFold(host, "config.toml") {
			return host
		}
		return "[host]"
	})
	value = diagnosticIP.ReplaceAllString(value, "[IP]")
	value = diagnosticToken.ReplaceAllString(value, "[REDACTED]")
	return security.SafeTruncate(strings.Join(strings.Fields(value), " "), 1000)
}

func sealUpstreamErrorDiagnostic(secret, userID, platform string, diagnostic upstreamErrorDiagnostic, random io.Reader) (string, error) {
	payload, err := json.Marshal(diagnostic)
	if err != nil || len(payload) > upstreamErrorDiagnosticSize-2 {
		return "", fmt.Errorf("invalid error diagnostic size")
	}
	key := hmac.New(sha256.New, []byte(secret))
	key.Write([]byte(upstreamErrorDiagnosticDomain))
	block, err := aes.NewCipher(key.Sum(nil))
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = io.ReadFull(random, nonce); err != nil {
		return "", err
	}
	plaintext := make([]byte, upstreamErrorDiagnosticSize)
	binary.BigEndian.PutUint16(plaintext, uint16(len(payload)))
	copy(plaintext[2:], payload)
	aad := strings.Join([]string{upstreamErrorDiagnosticDomain, diagnostic.RequestID, userID, platform}, "\n")
	return "v1." + base64.RawURLEncoding.EncodeToString(aead.Seal(nonce, nonce, plaintext, []byte(aad))), nil
}

func currentUpstreamErrorDiagnostic(c *gin.Context) *upstreamErrorDiagnosticState {
	if c == nil {
		return nil
	}
	value, _ := c.Get(upstreamErrorDiagnosticKey)
	state, _ := value.(*upstreamErrorDiagnosticState)
	if state == nil {
		return nil
	}
	if c.Request != nil {
		if audit := upstreamTraceFromContext(c.Request.Context()); audit != nil {
			audit.mu.Lock()
			current := audit.current
			audit.mu.Unlock()
			if state.attempt != current {
				return nil
			}
		}
	}
	// Final emission can happen after a long retry wait. Issue a fresh envelope
	// so the receiver's short replay window does not discard the final cause.
	if state.envelope != "" && state.diagnostic.IssuedAt < time.Now().Unix()-10 {
		value, _ := c.Get(newAPIPolicyMetaContextKey)
		if verified, ok := value.(verifiedNewAPIPolicyContext); ok && verified.APIKeyID == requestAPIKeyID(c) && verified.MetaVerified && verified.VerificationSecret != "" {
			state.diagnostic.IssuedAt = time.Now().Unix()
			state.envelope, _ = sealUpstreamErrorDiagnostic(verified.VerificationSecret, verified.Identity.UserID, verified.Platform, state.diagnostic, rand.Reader)
		}
	}
	return state
}

// Capture before public error normalization. No prompt, raw response body,
// request headers or upstream URL is carried to the administrator.
func captureUpstreamErrorDiagnostic(c *gin.Context, body []byte, status int, source, stage string) {
	if c == nil {
		return
	}
	parsed := gjson.ParseBytes(body)
	var message, code, kind string
	for _, path := range []string{"error", "response.error", "response.status_details.error", "detail", ""} {
		value := parsed
		if path != "" {
			value = parsed.Get(path)
		}
		if value.Get("message").Type == gjson.String {
			message, code, kind = value.Get("message").String(), value.Get("code").String(), value.Get("type").String()
			break
		}
		// Several HTTP backends (including their edge proxies) return a
		// string-valued error/detail instead of the OpenAI error object. Do not
		// discard their only explanation before generating the public 500.
		if value.Type == gjson.String && strings.TrimSpace(value.String()) != "" {
			message, code, kind = value.String(), parsed.Get("code").String(), parsed.Get("type").String()
			break
		}
	}
	if message == "" && !gjson.ValidBytes(body) {
		message = string(body)
	}
	if strings.TrimSpace(message) == "" {
		return
	}
	safeMessage := upstreamErrorSafeMessage(c, message)
	if state := currentUpstreamErrorDiagnostic(c); state != nil {
		// The same public copy can cross the privacy boundary more than once.
		public, _ := publicErrorMessage(code)
		if message == publicUpstreamFailureMessage || message == public || safeMessage == state.diagnostic.Message {
			return
		}
	}
	diagnostic := upstreamErrorDiagnostic{Message: safeMessage, Code: safeDiagnosticToken(upstreamErrorSafeMessage(c, code)), Type: safeDiagnosticToken(upstreamErrorSafeMessage(c, kind)), Source: source, Stage: stage, IssuedAt: time.Now().Unix()}
	if status >= 100 && status <= 599 {
		diagnostic.HTTPStatus = status
	}
	state := &upstreamErrorDiagnosticState{diagnostic: diagnostic}
	if c.Request != nil {
		snapshot := snapshotUpstreamTrace(c.Request.Context())
		state.diagnostic.GatewayRequestID = snapshot.RequestID
		if d := snapshot.Transport; d != nil {
			state.diagnostic.Transport = d.Transport
			state.diagnostic.HandshakeStatus = d.HandshakeStatus
			// WS adapters also return synthetic http.Response objects. An
			// attached observer is authoritative even when HTTPStatus is zero.
			state.diagnostic.HTTPStatus = d.HTTPStatus
			if d.ErrorSource != "" {
				state.diagnostic.Source = d.ErrorSource
			}
			if d.ErrorStage != "" {
				state.diagnostic.Stage = d.ErrorStage
			}
			state.diagnostic.UpstreamRequestID = safeDiagnosticToken(d.UpstreamRequestID)
		}
		if audit := upstreamTraceFromContext(c.Request.Context()); audit != nil {
			audit.mu.Lock()
			state.attempt = audit.current
			audit.mu.Unlock()
		}
	}
	value, _ := c.Get(newAPIPolicyMetaContextKey)
	if verified, ok := value.(verifiedNewAPIPolicyContext); ok && verified.APIKeyID == requestAPIKeyID(c) && verified.MetaVerified && verified.VerificationSecret != "" && verified.Meta.ChannelID > 0 {
		state.diagnostic.RequestID, state.diagnostic.ChannelID = verified.Identity.RequestID, verified.Meta.ChannelID
		state.envelope, _ = sealUpstreamErrorDiagnostic(verified.VerificationSecret, verified.Identity.UserID, verified.Platform, state.diagnostic, rand.Reader)
	}
	c.Set(upstreamErrorDiagnosticKey, state)
	// Also retain a bounded, redacted cause in local server logs for unbound clients.
	encoded, _ := json.Marshal(state.diagnostic)
	log.Printf("upstream_error_diagnostic %s", encoded)
}

func publishUpstreamErrorHeader(c *gin.Context) {
	if state := currentUpstreamErrorDiagnostic(c); state != nil && state.envelope != "" && c.Writer != nil && !c.Writer.Written() {
		c.Header(upstreamErrorDiagnosticHeader, state.envelope)
	}
}

func upstreamErrorEventPath(data []byte) string {
	value := gjson.ParseBytes(data)
	if value.Get("type").String() == "response.failed" && value.Get("response.error").IsObject() {
		return "response.error"
	}
	if (value.Get("type").String() == "error" || value.Get("type").String() == "") && value.Get("error").IsObject() {
		return "error"
	}
	if value.Get("type").String() == "error" && value.Get("message").Type == gjson.String {
		return "."
	}
	return ""
}

func upstreamErrorSSEComment(c *gin.Context, data []byte) string {
	state := currentUpstreamErrorDiagnostic(c)
	if state == nil || state.envelope == "" || upstreamErrorEventPath(data) == "" {
		return ""
	}
	return ": codex2api_error " + state.envelope + "\n\n"
}

func protectedUpstreamErrorWS(c *gin.Context, data []byte) []byte {
	state := currentUpstreamErrorDiagnostic(c)
	path := upstreamErrorEventPath(data)
	if state == nil || state.envelope == "" || path == "" {
		return data
	}
	field := path + ".details.codex2api_error"
	if path == "." {
		field = "details.codex2api_error"
	}
	result, err := sjson.SetBytes(data, field, state.envelope)
	if err != nil {
		return data
	}
	return result
}

func diagnosticWSClientUpstreamAPIError(c *gin.Context, apiErr *api.APIError, hide bool) *api.APIError {
	if apiErr != nil && currentUpstreamErrorDiagnostic(c) == nil {
		body, _ := json.Marshal(gin.H{"error": apiErr})
		captureUpstreamErrorDiagnostic(c, body, 0, "upstream_event", "response_error")
	}
	return responsesWSClientUpstreamAPIError(apiErr, hide)
}
