package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strings"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

// CodexBPSBaseURL is the upstream Basis Points endpoint used by the official
// BPS Word integration. It is intentionally separate from the native Codex
// endpoint so account routing and diagnostics cannot be mixed.
const CodexBPSBaseURL = "https://bps.openai.com/basispoints/api"

const bpsToolsVersion = "tools-word-core-2026-08-17-5b142653"

const bpsCallerRuntimeInstructions = "You are Codex running through the Basis Points transport. Preserve tool calls and return Responses API events."

// CodexBPSDiagnostic records the small amount of transport metadata useful to
// request tests and future usage attribution without exposing prompt content.
type CodexBPSDiagnostic struct {
	RequestedModel string   `json:"requested_model"`
	SentModel      string   `json:"sent_model"`
	Compact        bool     `json:"compact,omitempty"`
	AdaptedFields  []string `json:"adapted_fields,omitempty"`
	RemovedFields  []string `json:"removed_fields,omitempty"`
}

type codexBPSDiagnosticKey struct{}

// ExecuteCodexBPSProbe uses the account's configured transport for admin probes.
func ExecuteCodexBPSProbe(ctx context.Context, account *auth.Account, body []byte, proxyURL string) (*http.Response, error) {
	return executeCodexBPS(ctx, account, body, "", proxyURL, false)
}

func codexBPSDigest(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func prepareCodexBPSBody(body []byte, cacheKey string, compact bool) ([]byte, *CodexBPSDiagnostic, error) {
	var source map[string]json.RawMessage
	if err := json.Unmarshal(body, &source); err != nil {
		return nil, nil, err
	}
	d := &CodexBPSDiagnostic{
		RequestedModel: gjson.GetBytes(body, "model").String(),
		SentModel:      gjson.GetBytes(body, "model").String(),
		Compact:        compact,
	}
	if strings.EqualFold(strings.TrimSpace(d.RequestedModel), "codex-auto-review") {
		d.SentModel = "gpt-5.6-luna"
		d.AdaptedFields = append(d.AdaptedFields, "model: codex-auto-review -> gpt-5.6-luna")
	}

	items := make([]json.RawMessage, 0)
	input := gjson.GetBytes(body, "input")
	switch {
	case input.Type == gjson.String:
		item, _ := json.Marshal(map[string]any{
			"type": "message", "role": "user",
			"content": []map[string]string{{"type": "input_text", "text": input.String()}},
		})
		items = append(items, item)
	case input.IsArray():
		if err := json.Unmarshal(source["input"], &items); err != nil {
			return nil, nil, err
		}
	case input.Exists() && input.Type != gjson.Null:
		return nil, nil, &Error{Code: "invalid_request_error", Type: ErrorTypeInvalidRequest, HTTPStatus: http.StatusBadRequest, Message: "BPS 请求的 input 必须是文本或数组"}
	}

	prefix := make([]json.RawMessage, 0, 3)
	runtime, _ := json.Marshal(map[string]any{
		"type": "message", "role": "developer",
		"content": []map[string]string{{"type": "input_text", "text": bpsCallerRuntimeInstructions}},
	})
	prefix = append(prefix, runtime)
	if tools := gjson.GetBytes(body, "tools"); tools.IsArray() && len(tools.Array()) > 0 {
		item, _ := json.Marshal(map[string]any{"type": "additional_tools", "role": "developer", "tools": json.RawMessage(source["tools"])})
		prefix = append(prefix, item)
		d.AdaptedFields = append(d.AdaptedFields, "tools -> input.additional_tools")
	}
	if instructions := gjson.GetBytes(body, "instructions"); instructions.Type == gjson.String && instructions.String() != "" {
		item, _ := json.Marshal(map[string]any{
			"type": "message", "role": "developer",
			"content": []map[string]string{{"type": "input_text", "text": instructions.String()}},
		})
		prefix = append(prefix, item)
		d.AdaptedFields = append(d.AdaptedFields, "instructions -> input.developer")
	}
	items = append(prefix, items...)

	metadata := map[string]string{
		"task_id":              "task_" + codexBPSDigest("task", cacheKey),
		"turn_id":              "turn_" + codexBPSDigest("turn", cacheKey),
		"bps_tools_version_id": bpsToolsVersion,
		"agent_iteration":      "1",
	}
	result := map[string]any{"model": d.SentModel, "input": items, "metadata": metadata}
	if !compact {
		effort := gjson.GetBytes(body, "reasoning.effort").String()
		if effort == "" {
			effort = "low"
		}
		result["model_selection"] = "explicit"
		result["stream"] = gjson.GetBytes(body, "stream").Bool()
		result["store"] = false
		result["reasoning_effort"] = effort
		result["prompt_cache_key"] = cacheKey
	}
	d.AdaptedFields = append(d.AdaptedFields, "client_metadata -> BPS metadata")
	for key := range source {
		if key == "model" || key == "input" || key == "tools" || key == "instructions" || key == "reasoning" || key == "stream" || key == "metadata" {
			continue
		}
		d.RemovedFields = append(d.RemovedFields, key)
	}
	sort.Strings(d.RemovedFields)
	encoded, err := json.Marshal(result)
	return encoded, d, err
}

// executeCodexBPS sends a Responses request to BPS. BPS currently returns the
// same SSE/JSON event envelope as Responses, so the handler can reuse its
// existing streaming and usage pipeline without a second protocol translator.
func executeCodexBPS(ctx context.Context, account *auth.Account, body []byte, cacheKey, proxyOverride string, compact bool) (*http.Response, error) {
	if account == nil || !account.CodexBPSEnabled() {
		return nil, ErrNoAvailableAccount()
	}
	token := account.GetAccessToken()
	if token == "" {
		return nil, ErrNoAvailableAccount()
	}
	if strings.TrimSpace(cacheKey) == "" {
		cacheKey = NewUpstreamSessionUUID()
	}
	projected, diagnostic, err := prepareCodexBPSBody(body, cacheKey, compact)
	if err != nil {
		return nil, ErrInternalError("构建 BPS 请求失败", err)
	}
	endpoint := CodexBPSBaseURL + "/responses"
	if compact {
		endpoint += "/compact"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(projected))
	if err != nil {
		return nil, ErrInternalError("创建 BPS 请求失败", err)
	}
	applyCodexBPSHeaders(req.Header, account, token, cacheKey, compact)
	req.Header.Set("X-Codex2API-BPS-Requested-Model", diagnostic.RequestedModel)
	log.Printf("[CODEX-TRANSPORT] endpoint=%s account=%d bps=true compact=%t requested_model=%s sent_model=%s", endpoint, account.ID(), compact, diagnostic.RequestedModel, diagnostic.SentModel)
	proxyURL := account.GetProxyURL()
	if strings.TrimSpace(proxyOverride) != "" {
		proxyURL = proxyOverride
	}
	client := getPooledClient(account, proxyURL)
	resp, err := doTracedUpstreamRequest(client, req, account, proxyURL)
	if err != nil {
		if shouldRecyclePooledClient(err) {
			recyclePooledClient(account, proxyURL)
		}
		return nil, ErrUpstream(0, "请求 BPS 上游失败", err)
	}
	resp.Request = resp.Request.WithContext(context.WithValue(resp.Request.Context(), codexBPSDiagnosticKey{}, diagnostic))
	return resp, nil
}

func applyCodexBPSHeaders(headers http.Header, account *auth.Account, token, cacheKey string, compact bool) {
	headers.Set("Authorization", "Bearer "+token)
	headers.Set("Chatgpt-Account-Id", account.EffectiveAccountID())
	headers.Set("X-Openai-Account-Id", account.EffectiveAccountID())
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "text/event-stream")
	if compact {
		headers.Set("Accept", "application/json")
	}
	headers.Set("Origin", "https://bps.openai.com")
	headers.Set("Referer", "https://bps.openai.com/")
	headers.Set("Session-Id", cacheKey)
	headers.Set("X-Basispoints-Auth-Mode", "chatgpt")
	deviceID := "bps-" + codexBPSDigest("device", account.EffectiveAccountID())[:32]
	headers.Set("X-Openai-Internal-Basispoints-Client-Device-Id", deviceID)
	for key, value := range map[string]string{
		"Client-Agent-Profile": "document", "Client-Editor": "word", "Client-Host": "Word",
		"Client-Platform": "word", "Client-Platform-Class": "desktop", "Client-Product": "basispoints-word-plugin",
		"Client-Runtime": "officejs", "Office-Host": "Word", "Office-Host-Version": "16.113",
		"Office-Platform": "Mac", "Tools-Version-Id": bpsToolsVersion,
	} {
		headers.Set("X-Openai-Internal-Basispoints-"+key, value)
	}
	headers.Set("X-Openai-Internal-Codex-Responses-Lite", "true")
}

func IsCodexBPSEndpoint(endpoint string) bool {
	return strings.HasPrefix(endpoint, CodexBPSBaseURL+"/")
}

func CodexBPSResponseDiagnostic(resp *http.Response) *CodexBPSDiagnostic {
	if resp == nil || resp.Request == nil {
		return nil
	}
	diagnostic, _ := resp.Request.Context().Value(codexBPSDiagnosticKey{}).(*CodexBPSDiagnostic)
	return diagnostic
}
