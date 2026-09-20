package admin

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/tidwall/gjson"
)

// probeNativeAPIAccount validates inference using the provider's existing native
// executor, including explicitly enabled Grok/Antigravity OAuth validation.
// API keys never visit OAuth quota endpoints. Success uses ordinary traffic
// accounting and cannot override an operator pause or an active failure.
func (h *Handler) probeNativeAPIAccount(ctx context.Context, account *auth.Account) error {
	model, err := h.connectionTestModelForAccount(ctx, account, "")
	if err != nil {
		return err
	}
	started := time.Now()
	proxyURL := h.store.ResolveProxyForAccount(account)
	var resp *http.Response
	switch {
	case account.IsClaudeOAuth():
		body := []byte(fmt.Sprintf(`{"model":%q,"max_tokens":1,"messages":[{"role":"user","content":"ping"}],"stream":false}`, model))
		if h.executeClaudeUsageProbe != nil {
			resp, err = h.executeClaudeUsageProbe(ctx, account, body)
		} else {
			resp, err = proxy.ExecuteClaudeMessagesRequest(ctx, account, body, proxyURL, nil, account.EffectiveClaudeFingerprintMode(h.store.ClaudeFingerprintModeDefault()), h.store.ClaudeSecurityConfig())
		}
	case account.IsAntigravityAPI():
		resp, err = h.antigravityProbeExecutor()(ctx, account, model, []byte(`{"input":"Reply with OK.","max_output_tokens":1}`), false, proxyURL)
	case account.IsGrokAPI():
		origin, _ := account.GrokCredentials()
		resp, err = proxy.ExecuteGrokNativeProtocolProbeAtOriginWithHeaders(ctx, account, proxy.GrokProtocolResponses, model, proxy.MinimalGrokProbeBody(proxy.GrokProtocolResponses, model), origin, proxyURL, nil)
	default:
		body := buildConnectionTestPayload(h.store, model)
		if h.executeUsageProbe != nil {
			resp, err = h.executeUsageProbe(ctx, account, body, "", proxyURL, "", nil, nil)
		} else {
			resp, err = proxy.ExecuteOpenAIResponsesRequest(ctx, account, body, proxyURL, nil)
		}
	}
	if err != nil {
		return err
	}
	if resp == nil || resp.Body == nil {
		return fmt.Errorf("native probe returned no response")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil {
		return err
	}
	if len(body) > 2<<20 {
		return fmt.Errorf("native probe response exceeded limit")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := fmt.Sprintf("native probe returned %d: %s", resp.StatusCode, truncate(string(body), 300))
		if proxy.ApplyExplicitProbeError(h.store, account, resp.StatusCode, body, resp, model) {
			return fmt.Errorf("%s", message)
		}
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			if account.APIAutoRecoveryEnabledForAccount() {
				h.store.MarkAPIUpstreamUnavailable(account, time.Duration(retryAfterSeconds(resp.Header, time.Now()))*time.Second, message)
			} else if account.IsAPIKeyAccount() {
				h.store.MarkCooldownWithError(account, 24*time.Hour, "unauthorized", message)
			}
			// Optional OAuth observations do not impose Codex authentication bans
			// on Grok or Antigravity; their native state/renewal paths own that.
		case http.StatusPaymentRequired:
			// Explicit billing rejection remains an actionable billing constraint.
			h.store.MarkError(account, message)
		case http.StatusTooManyRequests:
			if account.IsAntigravityAPI() {
				proxy.ApplyAntigravityCooldown(h.store, account, resp.StatusCode, body, resp, model)
			} else if account.IsAPIKeyAccount() {
				h.store.MarkTransientRateLimited(account, time.Duration(retryAfterSeconds(resp.Header, time.Now()))*time.Second)
			} else if account.IsClaudeOAuth() {
				proxy.SyncClaudeUsageState(h.store, account, resp)
			} else if account.IsGrokAPI() {
				proxy.Apply429Cooldown(h.store, account, body, resp, model)
			}
		case http.StatusServiceUnavailable:
			if account.IsAntigravityAPI() {
				proxy.ApplyAntigravityCooldown(h.store, account, resp.StatusCode, body, resp, model)
			}
		}
		return fmt.Errorf("%s", message)
	}
	valid := false
	switch {
	case account.IsClaudeOAuth():
		valid = gjson.ValidBytes(body) && gjson.GetBytes(body, "type").String() == "message" && gjson.GetBytes(body, "content").IsArray() && !nativeProbeHasError(gjson.ParseBytes(body))
		if valid {
			proxy.SyncClaudeUsageState(h.store, account, resp)
		}
	case account.IsAntigravityAPI():
		valid = nativeResponsesProbeSucceeded(body)
		if !valid && gjson.ValidBytes(body) {
			envelope := gjson.ParseBytes(body)
			// A raw Interactions response must contain the actual result fields.
			valid = envelope.Get("id").String() != "" && envelope.Get("outputs").IsArray() && !nativeProbeHasError(envelope) &&
				(envelope.Get("status").String() == "" || envelope.Get("status").String() == "completed")
		}
	default:
		valid = nativeResponsesProbeSucceeded(body)
	}
	if !valid {
		return fmt.Errorf("native probe did not return a verified successful response")
	}
	h.store.ReportRequestSuccess(account, time.Since(started))
	return nil
}

func nativeProbeHasError(value gjson.Result) bool {
	return value.Get("error").Type != gjson.Null || value.Get("type").String() == "error"
}

// The probe intentionally requests a tiny output budget. Only the explicit
// max_output_tokens terminal reason proves successful validation when truncated.
func nativeResponsesProbeSucceeded(body []byte) bool {
	classify := func(data []byte) (terminal, success bool) {
		if !gjson.ValidBytes(data) {
			return false, false
		}
		root := gjson.ParseBytes(data)
		if nativeProbeHasError(root) {
			return true, false
		}
		kind := root.Get("type").String()
		if kind == "response.failed" || kind == "response.cancelled" || kind == "response.canceled" {
			return true, false
		}
		response := root
		if nested := root.Get("response"); nested.Exists() {
			response = nested
		}
		if nativeProbeHasError(response) {
			return true, false
		}
		switch response.Get("status").String() {
		case "completed":
			return true, kind == "" || kind == "response" || kind == "response.completed"
		case "incomplete":
			return true, (kind == "" || kind == "response" || kind == "response.incomplete") && response.Get("incomplete_details.reason").String() == "max_output_tokens"
		case "failed", "cancelled", "canceled":
			return true, false
		}
		return false, false
	}
	if gjson.ValidBytes(body) {
		_, ok := classify(body)
		return ok
	}
	valid := false
	err := proxy.ReadSSEStream(bytes.NewReader(body), func(data []byte) bool {
		terminal, ok := classify(data)
		if terminal {
			valid = ok
			return false
		}
		return true
	})
	return err == nil && valid
}
