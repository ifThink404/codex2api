package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

const CodexBPSBaseURL = "https://bps.openai.com/basispoints/api"
const bpsToolsVersion = "tools-word-core-2026-08-17-5b142653"

type CodexBPSDiagnostic struct {
	projection        *bpsResponseProjection
	UpstreamTurnState *usageTurnStateValue     `json:"upstream_turn_state,omitempty"`
	ClientTurnState   *usageTurnStateValue     `json:"client_turn_state,omitempty"`
	Mode              string                   `json:"mode"`
	RequestedModel    string                   `json:"requested_model"`
	SentModel         string                   `json:"sent_model"`
	Compact           bool                     `json:"compact,omitempty"`
	AdaptedFields     []string                 `json:"adapted_fields,omitempty"`
	RemovedFields     []string                 `json:"removed_fields,omitempty"`
	Images            *codexBPSImageDiagnostic `json:"images,omitempty"`
	Files             *codexBPSFileDiagnostic  `json:"files,omitempty"`
}
type codexBPSDiagnosticKey struct{}

func codexAccountUpstreamMode(account *auth.Account) string {
	if account.CodexBPSEnabled() {
		return "bps"
	}
	return "native"
}

// A committed root pins the route, including related/passive requests. An old
// record without this field means native; changing account settings never
// silently moves an existing conversation and its opaque history to BPS.
func codexRequestUsesBPS(ctx context.Context, account *auth.Account) (bool, error) {
	if mode, _ := ctx.Value(codexTestModeKey{}).(string); mode != "" && mode != "auto" {
		if err := ValidateCodexTestMode(ctx, account); err != nil {
			return false, err
		}
		return mode == "bps", nil
	}
	if account == nil || account.IsRelayStyle() || account.IsCodexAgentIdentity() {
		return false, nil
	}
	if epoch := outboundEpochFromContext(ctx); epoch != nil && epoch.record.AccountID > 0 {
		return epoch.record.UpstreamMode == "bps", nil
	}
	if s, _ := ctx.Value(protocolIdentityKey{}).(*responseIdentitySession); s != nil && s.root != "" {
		entry, found, err := s.handler.readSessionContinuity(ctx, s.rootKey)
		if err != nil {
			return false, codexAccountIdentityError("无法确认会话的上游请求模式，请稍后重试。")
		}
		if found {
			return entry.Record.UpstreamMode == "bps", nil
		}
	}
	return account.CodexBPSEnabled(), nil
}

func prepareCodexBPSBody(body []byte, cacheKey string, compact bool) ([]byte, *CodexBPSDiagnostic, error) {
	var source map[string]json.RawMessage
	if err := json.Unmarshal(body, &source); err != nil {
		return nil, nil, err
	}
	model := gjson.GetBytes(body, "model").String()
	d := &CodexBPSDiagnostic{Mode: "bps", RequestedModel: model, SentModel: model, Compact: compact}
	if strings.EqualFold(strings.TrimSpace(model), "codex-auto-review") {
		d.SentModel = "gpt-5.6-luna"
		d.AdaptedFields = append(d.AdaptedFields, "model: codex-auto-review → gpt-5.6-luna")
	}
	var items []json.RawMessage
	input := gjson.GetBytes(body, "input")
	if input.Type == gjson.String {
		message, _ := json.Marshal(map[string]any{"type": "message", "role": "user", "content": input.String()})
		items = append(items, message)
	} else if input.IsArray() {
		if err := json.Unmarshal(source["input"], &items); err != nil {
			return nil, nil, err
		}
	} else if input.Exists() && input.Type != gjson.Null {
		return nil, nil, &Error{Code: "invalid_request_error", Type: ErrorTypeInvalidRequest, HTTPStatus: 400, Message: "BPS 请求的 input 必须是文本或数组"}
	}
	var prefix []json.RawMessage
	runtimeMessage, _ := json.Marshal(map[string]any{"type": "message", "role": "developer", "content": []map[string]string{{"type": "input_text", "text": bpsCallerRuntimeInstructions}}})
	prefix = append(prefix, runtimeMessage)
	if tools := gjson.GetBytes(body, "tools"); tools.IsArray() && len(tools.Array()) > 0 {
		item, _ := json.Marshal(map[string]any{"type": "additional_tools", "role": "developer", "tools": source["tools"]})
		prefix = append(prefix, item)
		d.AdaptedFields = append(d.AdaptedFields, "tools → input.additional_tools")
	}
	if instructions := gjson.GetBytes(body, "instructions"); instructions.Type == gjson.String && instructions.String() != "" {
		item, _ := json.Marshal(map[string]any{"type": "message", "role": "developer", "content": []map[string]string{{"type": "input_text", "text": instructions.String()}}})
		prefix = append(prefix, item)
		d.AdaptedFields = append(d.AdaptedFields, "instructions → input.developer")
	}
	items = append(prefix, items...)
	items, d.Images = normalizeBPSInputImages(items)
	if d.Images != nil && d.Images.MIMENormalized > 0 {
		d.AdaptedFields = append(d.AdaptedFields, "input image MIME normalized")
	}
	var imageErr error
	items, imageErr = projectBPSCustomImageOutputs(items, d)
	if imageErr != nil {
		return nil, nil, imageErr
	}
	if items == nil {
		items = []json.RawMessage{}
	}
	// These seeds have already passed account identity mapping. Domain separation
	// keeps BPS identifiers/cache state separate from the native Codex transport.
	metadataHeaders := CodexRequestMetadataHeaders(nil, body)
	turnSeed := gjson.Get(metadataHeaders.Get(codexTurnMetadataHeader), "turn_id").String()
	if turnSeed == "" {
		turnSeed = NewUpstreamSessionUUID()
	}
	taskSeed := gjson.Get(metadataHeaders.Get(codexTurnMetadataHeader), "session_id").String()
	if taskSeed == "" {
		taskSeed = cacheKey
	}
	metadata := map[string]string{
		"task_id":              "task_" + codexIdentityDigest("bps-task-v1", cacheKey, taskSeed),
		"turn_id":              "turn_" + codexIdentityDigest("bps-turn-v1", cacheKey, turnSeed),
		"bps_tools_version_id": bpsToolsVersion, "agent_iteration": "1",
	}
	result := map[string]any{"model": d.SentModel, "input": items, "metadata": metadata}
	if !compact {
		effort := gjson.GetBytes(body, "reasoning.effort").String()
		if effort == "" {
			effort = "low"
		}
		result["model_selection"], result["stream"], result["store"] = "explicit", true, false
		result["reasoning_effort"], result["prompt_cache_key"] = effort, cacheKey
		if _, found := source["reasoning"]; found {
			d.AdaptedFields = append(d.AdaptedFields, "reasoning.effort → reasoning_effort")
		}
	}
	d.AdaptedFields = append(d.AdaptedFields, "client_metadata → BPS metadata")
	for key := range source {
		if _, preserved := result[key]; preserved {
			continue
		}
		if key == "instructions" || key == "tools" || key == "client_metadata" || key == "reasoning" && !compact {
			continue
		}
		d.RemovedFields = append(d.RemovedFields, key)
	}
	if _, exists := source["metadata"]; exists {
		d.AdaptedFields = append(d.AdaptedFields, "metadata → BPS metadata")
	}
	sort.Strings(d.RemovedFields)
	encoded, err := json.Marshal(result)
	return encoded, d, err
}

func executeCodexBPS(ctx context.Context, account *auth.Account, body []byte, cacheKey, proxyOverride, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, fingerprint *CodexFingerprint, compact bool) (*http.Response, error) {
	account.Mu().RLock()
	accessToken, proxyURL := account.AccessToken, account.ProxyURL
	account.Mu().RUnlock()
	if accessToken == "" {
		return nil, ErrNoAvailableAccount()
	}
	if proxyOverride != "" {
		proxyURL = proxyOverride
	}
	if err := ValidateSessionOutboundRequest(ctx, account, body); err != nil {
		return nil, err
	}
	// First complete the shared privacy pass; no original client header is copied
	// to BPS. In particular, native Turn-State and routing headers cannot cross.
	body, headers = FinalizeCodexOutboundMetadata(body, headers, ctx)
	profileRequest, _ := http.NewRequestWithContext(ctx, http.MethodPost, CodexBPSBaseURL+"/responses", nil)
	applyCodexRequestHeaders(profileRequest, account, accessToken, cacheKey, apiKey, deviceCfg, headers, fingerprint)
	_, profileRequest.Header = FinalizeCodexOutboundMetadata(body, profileRequest.Header, ctx)
	var err error
	profileRequest.Header, err = FinalizeCodexURLHeaders(ctx, profileRequest.Header)
	if err != nil {
		return nil, err
	}
	if err = ValidateCodexOutboundMetadata(body, profileRequest.Header); err != nil {
		return nil, err
	}
	if cacheKey == "" {
		cacheKey = NewUpstreamSessionUUID()
	}
	cacheKey = codexIdentityDigest("bps-prompt-cache-v1", account.EffectiveAccountID(), cacheKey)
	projected, diagnostic, err := prepareCodexBPSBody(body, cacheKey, compact)
	if err != nil {
		if _, ok := err.(*Error); ok {
			return nil, err
		}
		return nil, ErrInternalError("构建 BPS 请求失败", err)
	}
	ctx = context.WithValue(ctx, codexBPSDiagnosticKey{}, diagnostic)
	diagnostic.projection = newBPSResponseProjection(body)
	endpoint := CodexBPSBaseURL + "/responses"
	if compact {
		endpoint += "/compact"
	}
	client := getPooledClient(account, proxyURL)
	if IsResinEnabled() {
		endpoint = BuildReverseProxyURL(endpoint)
		client = getResinHTTPClient(account)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(projected))
	if err != nil {
		return nil, ErrInternalError("创建 BPS 请求失败", err)
	}
	for _, name := range []string{"User-Agent", "Version", "Originator"} {
		if value := profileRequest.Header.Get(name); value != "" {
			req.Header.Set(name, value)
		}
	}
	applyCodexBPSHeaders(req.Header, account, accessToken, cacheKey, compact)
	if IsResinEnabled() {
		req.Header.Set("X-Resin-Account", ResinAccountID(account))
	}
	// Charge the requested model quota before sending upstream.
	if err := ConsumeAPIKeyModelRequestQuota(ctx, diagnostic.RequestedModel); err != nil {
		return nil, err
	}
	originalProjected, requestHeaders := projected, req.Header.Clone()
	for attempt := 0; ; attempt++ {
		var used map[string]string
		projected, used, err = prepareBPSUserImageAttachments(ctx, account, originalProjected, diagnostic, func(uploadCtx context.Context, data []byte, mime string) (string, error) {
			return uploadBPSImage(uploadCtx, client, requestHeaders, data, mime)
		})
		if err != nil {
			return nil, err
		}
		var filesUsed map[string]string
		projected, filesUsed, err = prepareBPSFileAttachments(ctx, account, projected, diagnostic, func(uploadCtx context.Context, file bpsFileAttachment) (string, error) {
			return uploadBPSAttachment(uploadCtx, client, requestHeaders, file)
		})
		if err != nil {
			return nil, err
		}
		for key, id := range filesUsed {
			used[key] = id
		}
		projected, err = bridgeBPSToolAttachments(projected, diagnostic)
		if err != nil {
			return nil, ErrInternalError("构建工具附件引用失败", err)
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(projected))
		if err != nil {
			return nil, ErrInternalError("创建上游请求失败", err)
		}
		req.Header = requestHeaders.Clone()
		resp, sendErr := doTracedUpstreamRequest(client, req, account, proxyURL, projected)
		if sendErr != nil {
			if shouldRecyclePooledClient(sendErr) {
				recyclePooledClient(account, proxyURL)
			}
			return nil, ErrUpstream(0, "请求上游失败", sendErr)
		}
		if attempt == 0 && invalidateMissingBPSAttachments(resp, used) {
			resp.Body.Close()
			continue
		}
		return resp, nil
	}
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
	deviceID := account.EffectiveCodexInstallationID()
	if deviceID == "" {
		deviceID = codexIdentityDigest("bps-device-v1", account.EffectiveAccountID())
	}
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

// Used by the admin connection test as well as usage/error diagnostics.
func IsCodexBPSEndpoint(endpoint string) bool {
	return strings.HasPrefix(endpoint, CodexBPSBaseURL+"/")
}

func CodexBPSResponseDiagnostic(resp *http.Response) *CodexBPSDiagnostic {
	if resp == nil || resp.Request == nil {
		return nil
	}
	d, _ := resp.Request.Context().Value(codexBPSDiagnosticKey{}).(*CodexBPSDiagnostic)
	return d
}

func applyBPSUsageTransport(input *database.UsageLogInput, d *UpstreamTransportDiagnostic) {
	if d == nil || d.BPS == nil {
		return
	}
	input.UpstreamEndpoint = CodexBPSBaseURL + "/responses"
	if d.BPS.Compact {
		input.UpstreamEndpoint += "/compact"
	}
	input.ViaWebsocket = false
}
