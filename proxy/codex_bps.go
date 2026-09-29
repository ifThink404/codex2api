package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy/plugins"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const CodexBPSBaseURL = "https://bps.openai.com/basispoints/api"
const bpsToolsVersion = "tools-word-core-2026-08-17-5b142653"

type CodexBPSDiagnostic struct {
	WordIdentity             *bpsWordIdentityDiagnostic    `json:"word_identity,omitempty"`
	FullConvergence          *bpsWordIdentityDiagnostic    `json:"full_convergence,omitempty"`
	RoundConvergence         *bpsWordIdentityDiagnostic    `json:"round_convergence,omitempty"`
	TurnConvergence          *bpsWordIdentityDiagnostic    `json:"turn_convergence,omitempty"`
	InferredSession          *inferredBPSSessionDiagnostic `json:"inferred_session,omitempty"`
	projection               *bpsResponseProjection
	ToolNamespaceRepair      *bpsNamespaceRepairDiagnostic   `json:"tool_namespace_repair,omitempty"`
	Timing                   *bpsTimingDiagnostic            `json:"timing,omitempty"`
	Usage                    *bpsUsageDiagnostic             `json:"usage_billing,omitempty"`
	Profile                  auth.CodexBPSProfile            `json:"profile"`
	ToolsVersion             string                          `json:"tools_version"`
	Mode                     string                          `json:"mode"`
	AgentIteration           string                          `json:"agent_iteration,omitempty"`
	RequestedModel           string                          `json:"requested_model"`
	SentModel                string                          `json:"sent_model"`
	RequestedReasoningEffort string                          `json:"requested_reasoning_effort,omitempty"`
	SentReasoningEffort      string                          `json:"sent_reasoning_effort,omitempty"`
	Compact                  bool                            `json:"compact,omitempty"`
	AdaptedFields            []string                        `json:"adapted_fields,omitempty"`
	RemovedFields            []string                        `json:"removed_fields,omitempty"`
	Images                   *codexBPSImageDiagnostic        `json:"images,omitempty"`
	Files                    *codexBPSFileDiagnostic         `json:"files,omitempty"`
	ImageHistory             *codexBPSImageHistoryDiagnostic `json:"image_history,omitempty"`
}
type codexBPSDiagnosticKey struct{}

func prepareCodexBPSBody(body []byte, cacheKey string, compact bool) ([]byte, *CodexBPSDiagnostic, error) {
	return prepareCodexBPSBodyWithImageTrim(body, cacheKey, compact, false, nil)
}

func prepareCodexBPSBodyWithImageTrim(body []byte, cacheKey string, compact, trimImages bool, headers http.Header) ([]byte, *CodexBPSDiagnostic, error) {
	return prepareCodexBPSBodyForProfile(body, cacheKey, compact, trimImages, headers, bpsProfile(auth.BPSWord))
}

func prepareCodexBPSBodyForProfile(body []byte, cacheKey string, compact, trimImages bool, headers http.Header, profile bpsProfileConfig, contexts ...context.Context) ([]byte, *CodexBPSDiagnostic, error) {
	var source map[string]json.RawMessage
	if err := json.Unmarshal(body, &source); err != nil {
		return nil, nil, err
	}
	model := gjson.GetBytes(body, "model").String()
	d := &CodexBPSDiagnostic{Mode: "bps", RequestedModel: model, SentModel: model, Compact: compact, Profile: profile.profile, ToolsVersion: profile.toolsVersion}
	repaired, repair, err := repairBPSToolNamespaces(body)
	if err != nil {
		return nil, nil, err
	}
	if repair.Repaired > 0 || repair.Unresolved > 0 || repair.ScanTruncated {
		d.ToolNamespaceRepair = repair
	}
	if repair.Repaired > 0 {
		body = repaired
		source["input"] = json.RawMessage(gjson.GetBytes(body, "input").Raw)
		d.AdaptedFields = append(d.AdaptedFields, "tool call namespace restored from unique declaration")
	}
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
	// A configuration_update can change effort after the request baseline. Only
	// rewrite protocol items, never messages, tool data, or JSON inside strings.
	configurationMapped := false
	configurationTierRemoved := false
	for i, item := range items {
		if gjson.GetBytes(item, "type").String() != "configuration_update" {
			continue
		}
		if requested := gjson.GetBytes(item, "reasoning.effort"); requested.Type == gjson.String && strings.TrimSpace(requested.String()) != "" {
			if effort := normalizeBPSReasoningEffort(requested.String()); effort != requested.String() {
				updated, err := sjson.SetBytes(item, "reasoning.effort", effort)
				if err != nil {
					return nil, nil, err
				}
				item = updated
				configurationMapped = true
			}
		}
		// BPS omits top-level service_tier below. A continuation must not
		// reintroduce fast/flex through a protocol configuration update.
		if gjson.GetBytes(item, "service_tier").Exists() {
			updated, err := sjson.DeleteBytes(item, "service_tier")
			if err != nil {
				return nil, nil, err
			}
			item = updated
			configurationTierRemoved = true
		}
		items[i] = item
	}
	if configurationMapped {
		d.AdaptedFields = append(d.AdaptedFields, "input.configuration_update.reasoning.effort normalized to a BPS tier")
	}
	if configurationTierRemoved {
		d.RemovedFields = append(d.RemovedFields, "input.configuration_update.service_tier")
	}
	var prefix []json.RawMessage
	runtimeMessage, _ := json.Marshal(map[string]any{"type": "message", "role": "developer", "content": []map[string]string{{"type": "input_text", "text": profile.runtimeInstructions()}}})
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
	if format := gjson.GetBytes(body, "text.format"); !compact && format.IsObject() {
		contract, err := bpsOutputFormatContract(format)
		if err != nil {
			return nil, nil, err
		}
		if contract != "" {
			item, _ := json.Marshal(map[string]any{"type": "message", "role": "developer", "content": []map[string]string{{"type": "input_text", "text": contract}}})
			prefix = append(prefix, item)
			d.AdaptedFields = append(d.AdaptedFields, "text.format → input.developer output contract (not enforced)")
		}
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
	if trimImages {
		items, imageErr = trimBPSImageHistory(items, body, headers, compact, d)
		if imageErr != nil {
			return nil, nil, imageErr
		}
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
		"bps_tools_version_id": profile.toolsVersion, "agent_iteration": bpsAgentIteration(input),
	}
	ctx := context.Background()
	if len(contexts) > 0 && contexts[0] != nil {
		ctx = contexts[0]
	}
	if profile.profile == auth.BPSWord || bpsFullConvergenceFrom(ctx) != nil {
		identity, err := resolveBPSWordIdentity(ctx, body, headers, cacheKey, d.SentModel, compact)
		if err != nil {
			return nil, nil, err
		}
		if profile.profile == auth.BPSWord {
			d.WordIdentity = identity
		}
		if scope := bpsFullConvergenceFrom(ctx); scope != nil {
			if scope.turnRoundLimit > 0 {
				d.TurnConvergence = identity
			} else if scope.roundLimit > 0 {
				d.RoundConvergence = identity
			} else {
				d.FullConvergence = identity
			}
		}
		metadata["task_id"], metadata["turn_id"], metadata["agent_iteration"] = identity.TaskID, identity.TurnID, identity.AgentIteration
	}
	d.AgentIteration = metadata["agent_iteration"]
	result := map[string]any{"model": d.SentModel, "input": items, "metadata": metadata}
	if !compact {
		effort := extractReasoningEffort(body)
		d.RequestedReasoningEffort = effort
		if strings.TrimSpace(effort) == "" {
			effort = defaultCodexReasoningEffort
			d.AdaptedFields = append(d.AdaptedFields, "reasoning_effort: absent → low")
		} else if normalized := normalizeBPSReasoningEffort(effort); normalized != effort {
			d.AdaptedFields = append(d.AdaptedFields, "reasoning_effort: "+bpsEffortLabel(effort)+" → "+normalized)
			effort = normalized
		}
		d.SentReasoningEffort = effort
		result["model_selection"], result["stream"], result["store"] = "explicit", true, false
		result["reasoning_effort"] = effort
		if profile.profile == auth.BPSWord {
			if value, present := source["context_management"]; present {
				result["context_management"] = value
			}
		} else {
			result["prompt_cache_key"] = cacheKey
		}
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

// executeCodexBPS sends one prepared Responses body to BPS. Called only from
// the BPS transport plugin. Relative to fj-server: the session outbound
// validation/finalization passes (group A) are dropped, the native request
// headers come from the raw downstream headers instead of a fingerprint, and
// transport diagnostics go to the plugin capture store and upstream trace.
func executeCodexBPS(ctx context.Context, svc plugins.Services, account *auth.Account, body []byte, cacheKey, proxyOverride, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, compact bool) (*http.Response, error) {
	ctx, releasePreparation := withBPSAttachmentPreparation(ctx)
	defer releasePreparation()
	ctx = withBPSUploadRequest(ctx)
	ctx = context.WithValue(ctx, bpsUploadAccountKey{}, account.ID())
	ctx = withBPSAttachmentFallback(ctx, account)
	started := time.Now()
	accessToken := account.GetAccessToken()
	proxyURL := account.GetProxyURL()
	profile := bpsProfile(account.EffectiveCodexBPSProfile())
	trimImages := bpsImageTrimEnabled(account)
	if accessToken == "" {
		return nil, ErrNoAvailableAccount()
	}
	if proxyOverride != "" {
		proxyURL = proxyOverride
	}
	// No original client header is copied to BPS; only the resolved native
	// identity headers (UA, Version, Originator) are reused below.
	profileRequest, _ := http.NewRequestWithContext(ctx, http.MethodPost, CodexBPSBaseURL+"/responses", nil)
	applyCodexRequestHeaders(profileRequest, account, accessToken, cacheKey, apiKey, deviceCfg, headers)
	var err error
	cacheKey, inferredSession := inferredBPSCacheSeed(ctx, account, cacheKey, compact)
	if cacheKey == "" {
		cacheKey = NewUpstreamSessionUUID()
	}
	cacheKey = bpsProfileCacheKey(account.EffectiveAccountID(), cacheKey, profile)
	ctx, err = withBPSFullConvergence(ctx, account, profile, headers, cacheKey, apiKey)
	if err != nil {
		return nil, err
	}
	if profile.profile == auth.BPSWord {
		owner := verifiedTransportUser(ctx)
		if owner == "" {
			owner = codexIdentityDigest("bps-word-api-key", apiKey)
		}
		ctx = context.WithValue(ctx, bpsWordAccountScopeKey{}, codexIdentityDigest("bps-word-owner-account", owner, account.EffectiveAccountID()))
	}
	projected, diagnostic, err := prepareCodexBPSBodyForProfile(body, cacheKey, compact, trimImages, headers, profile, ctx)
	if err != nil {
		if _, ok := err.(*Error); ok {
			return nil, err
		}
		return nil, ErrInternalError("构建 BPS 请求失败", err)
	}
	diagnostic.InferredSession = inferredSession
	ctx = context.WithValue(ctx, codexBPSDiagnosticKey{}, diagnostic)
	diagnostic.Timing = &bpsTimingDiagnostic{started: started, values: bpsTimingValues{FirstTokenModeAtStart: currentFirstTokenMode()}}
	ctx = context.WithValue(ctx, bpsTimingContextKey{}, diagnostic.Timing)
	storeBPSAttemptDiagnostic(ctx, diagnostic)
	// Preparation errors happen before the inference transport exists; record
	// the attempt against this account so usage rows attribute it correctly.
	preparationFailed := func(string) {
		beginUpstreamTrace(ctx, account, proxyURL, false)
	}
	diagnostic.projection = newBPSResponseProjection(body)
	endpoint := CodexBPSBaseURL + "/responses"
	if compact {
		endpoint += "/compact"
	}
	client, rewriteURL := svc.HTTPClient(account, proxyURL)
	endpoint = rewriteURL(endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(projected))
	if err != nil {
		return nil, ErrInternalError("创建 BPS 请求失败", err)
	}
	for _, name := range []string{"User-Agent", "Version", "Originator"} {
		if value := profileRequest.Header.Get(name); value != "" {
			req.Header.Set(name, value)
		}
	}
	applyCodexBPSHeadersForProfile(req.Header, account, accessToken, cacheKey, compact, profile)
	// The shared Codex profile was audited before the BPS headers replaced it.
	// Persist the final UA used by both attachment uploads and inference.
	svc.RecordUserAgent(ctx, req.Header.Get("User-Agent"))
	svc.PrepareRequest(req, account)
	// Charge the requested model quota before sending upstream.
	if err := svc.ConsumeModelQuota(ctx, diagnostic.RequestedModel); err != nil {
		return nil, err
	}
	originalProjected, requestHeaders := projected, req.Header.Clone()
	ctx, ladder := withBPSImageLadder(ctx)
	for attempt := 0; ; attempt++ {
		var used map[string]string
		phaseStarted := time.Now()
		projected, used, err = prepareBPSUserImageAttachments(ctx, account, originalProjected, diagnostic, func(uploadCtx context.Context, data []byte, mime string) (string, error) {
			return uploadBPSImage(uploadCtx, client, requestHeaders, data, mime)
		})
		diagnostic.Timing.update(func(v *bpsTimingValues) { v.ImagePrepareMS += time.Since(phaseStarted).Milliseconds() })
		if err != nil {
			preparationFailed("bps_image_preparation")
			return nil, err
		}
		var filesUsed map[string]string
		phaseStarted = time.Now()
		projected, filesUsed, err = prepareBPSFileAttachments(ctx, account, projected, diagnostic, func(uploadCtx context.Context, file bpsFileAttachment) (string, error) {
			return uploadBPSAttachment(uploadCtx, client, requestHeaders, file)
		})
		diagnostic.Timing.update(func(v *bpsTimingValues) { v.FilePrepareMS += time.Since(phaseStarted).Milliseconds() })
		if err != nil {
			preparationFailed("bps_file_preparation")
			return nil, err
		}
		for key, id := range filesUsed {
			used[key] = id
		}
		phaseStarted = time.Now()
		projected, err = bridgeBPSToolAttachments(projected, diagnostic)
		if err == nil {
			projected, err = bridgeBPSFallbackImages(projected, diagnostic)
		}
		diagnostic.Timing.update(func(v *bpsTimingValues) { v.ToolBridgeMS += time.Since(phaseStarted).Milliseconds() })
		if err != nil {
			preparationFailed("bps_tool_attachment_bridge")
			return nil, ErrInternalError("构建工具附件引用失败", err)
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(projected))
		if err != nil {
			return nil, ErrInternalError("创建上游请求失败", err)
		}
		req.Header = requestHeaders.Clone()
		if err := touchBPSRoundIdentity(ctx, diagnostic.RoundConvergence); err != nil {
			preparationFailed("bps_round_activity")
			return nil, ErrInternalError("记录 BPS 会话首次发送时间失败", err)
		}
		if err := touchBPSTurnIdentity(ctx, diagnostic.TurnConvergence); err != nil {
			preparationFailed("bps_turn_activity")
			return nil, ErrInternalError("记录 BPS 任务发送时间失败", err)
		}
		if env, _ := ctx.Value(bpsAttemptEnvKey{}).(*plugins.ReqEnv); env != nil {
			env.CaptureUpstreamRequest(req.Header, projected)
		}
		// BPS timing (fj recorded it inside its transport observer).
		diagnostic.Timing.startInference(time.Now())
		traced, network := traceBPSHTTP(req)
		resp, sendErr := svc.Do(client, traced, account, proxyURL)
		phases := network.finish(sendErr)
		diagnostic.Timing.update(func(v *bpsTimingValues) { v.LastInferenceHTTP = phases })
		if resp != nil {
			diagnostic.Timing.receivedHeaders(time.Now())
		}
		if sendErr != nil {
			svc.RecycleClient(account, proxyURL, sendErr)
			return nil, ErrUpstream(0, "请求上游失败", sendErr)
		}
		if attempt == 0 && invalidateMissingBPSAttachments(resp, used) {
			diagnostic.Timing.update(func(v *bpsTimingValues) { v.AttachmentRetries++ })
			resp.Body.Close()
			continue
		}
		if bpsImageRefusal(resp) && ladder.advance(ctx) {
			diagnostic.Timing.update(func(v *bpsTimingValues) { v.AttachmentRetries++ })
			resp.Body.Close()
			continue
		}
		return resp, nil
	}
}

func applyCodexBPSHeaders(headers http.Header, account *auth.Account, token, cacheKey string, compact bool) {
	applyCodexBPSHeadersForProfile(headers, account, token, cacheKey, compact, bpsProfile(account.EffectiveCodexBPSProfile()))
}

func applyCodexBPSHeadersForProfile(headers http.Header, account *auth.Account, token, cacheKey string, compact bool, profile bpsProfileConfig) {
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
	if profile.profile == auth.BPSWord {
		for _, name := range []string{"Version", "Originator", "Session-Id", "X-Openai-Internal-Basispoints-Client-Device-Id", "X-Openai-Internal-Basispoints-Tools-Version-Id", "X-Openai-Internal-Codex-Responses-Lite", "X-Openai-Internal-Basispoints-Office-Host-Version"} {
			headers.Del(name)
		}
		headers.Set("User-Agent", bpsWordUserAgent())
		profile.applyHeaders(headers)
		return
	}
	// fj preferred the account's persisted Codex installation ID, which this
	// gateway does not track; the account-scoped digest is fj's own fallback.
	deviceID := codexIdentityDigest("bps-device-v1", account.EffectiveAccountID())
	headers.Set("X-Openai-Internal-Basispoints-Client-Device-Id", deviceID)
	profile.applyHeaders(headers)
	headers.Set("X-Openai-Internal-Codex-Responses-Lite", "true")
}

// Used by the admin connection test as well as usage/error diagnostics.
func IsCodexBPSEndpoint(endpoint string) bool {
	return strings.HasPrefix(endpoint, CodexBPSBaseURL+"/")
}

// CodexBPSResponseDiagnostic returns the BPS request diagnostic of a response
// produced by executeCodexBPS (nil for other responses).
func CodexBPSResponseDiagnostic(resp *http.Response) *CodexBPSDiagnostic {
	if resp == nil || resp.Request == nil {
		return nil
	}
	d, _ := resp.Request.Context().Value(codexBPSDiagnosticKey{}).(*CodexBPSDiagnostic)
	return d
}
