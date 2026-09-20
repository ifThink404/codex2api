package proxy

import (
	"sort"
	"strings"

	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

var diagnosticDeviceFields = []string{
	"installation_id", "installationId", "device_id", "deviceId",
	"x-codex-installation-id", "x_codex_installation_id", "x-device-id", "x_device_id",
	"client_name", "client_version", "os_name", "os_version", "arch", "timezone",
}

var diagnosticClientHeaders = []string{
	"X-Codex-Installation-Id", "X-Installation-Id", "X-Device-Id", "Oai-Device-Id",
	"User-Agent", "Originator", "Version", "X-Stainless-OS", "X-Stainless-Arch",
	"X-Stainless-Runtime", "X-Stainless-Runtime-Version", "X-Stainless-Package-Version",
}

type usageRequestInfo struct {
	Method              string `json:"method,omitempty"`
	Endpoint            string `json:"endpoint,omitempty"`
	Transport           string `json:"transport,omitempty"`
	Model               string `json:"model,omitempty"`
	EffectiveModel      string `json:"effective_model,omitempty"`
	RequestID           string `json:"request_id,omitempty"`
	UpstreamRequestID   string `json:"upstream_request_id,omitempty"`
	APIKeyID            int64  `json:"api_key_id,omitempty"`
	APIKeyName          string `json:"api_key_name,omitempty"`
	Stream              bool   `json:"stream"`
	UpstreamViaWS       bool   `json:"upstream_via_websocket"`
	ReasoningEffort     string `json:"reasoning_effort,omitempty"`
	ClientUserAgent     string `json:"client_user_agent,omitempty"`
	UpstreamUserAgent   string `json:"upstream_user_agent,omitempty"`
	UserAgentOverridden *bool  `json:"user_agent_overridden,omitempty"`
}

func diagnosticClientText(value string) string {
	value = strings.TrimSpace(value)
	lower := strings.ToLower(value)
	if strings.HasPrefix(lower, "sk-") || strings.HasPrefix(lower, "eyj") {
		return diagnosticIdentifier(value)
	}
	value = security.SafeTruncate(strings.TrimSpace(value), 2048)
	return strings.Clone(security.SafeTruncate(security.MaskSensitiveData(value), 512))
}

func captureUsageDiagnosticHeaders(ctx *gin.Context) map[string]string {
	headers := make(map[string]string)
	if ctx == nil || ctx.Request == nil {
		return headers
	}
	names := []string{
		"Session-Id", "Session_id", "Conversation-Id", "Thread-Id", "X-Client-Request-Id",
		"X-Codex-Window-Id", "X-Codex-Parent-Thread-Id", "X-Codex-Forked-From-Thread-Id", "X-OpenAI-Subagent",
		"X-Codex-Project-Id", "X-Codex-Workspace-Id",
	}
	names = append(names, diagnosticClientHeaders...)
	for _, name := range names {
		values := ctx.Request.Header.Values(name)
		if len(values) == 0 {
			continue
		}
		switch name {
		case "User-Agent", "Originator", "Version", "X-Stainless-OS", "X-Stainless-Arch", "X-Stainless-Runtime", "X-Stainless-Runtime-Version", "X-Stainless-Package-Version":
			headers[name] = diagnosticClientText(serviceErrorSafeText(ctx, values[0], 512))
		case "X-OpenAI-Subagent":
			headers[name] = diagnosticLabel(values[0])
		default:
			headers[name] = diagnosticIdentifier(values[0])
		}
		if len(values) > 1 {
			headers[name+"_multiple"] = "true"
		}
	}
	return headers
}

func usageRequestInfoSnapshot(ctx *gin.Context, input *database.UsageLogInput) *usageRequestInfo {
	info := &usageRequestInfo{
		Model: diagnosticLabel(input.Model), EffectiveModel: diagnosticLabel(input.EffectiveModel),
		RequestID: diagnosticRequestID(input.RequestID), UpstreamRequestID: diagnosticRequestID(input.UpstreamRequestID),
		APIKeyID: input.APIKeyID, APIKeyName: serviceErrorSafeText(ctx, input.APIKeyName, 160),
		Stream: input.Stream, UpstreamViaWS: input.ViaWebsocket, ReasoningEffort: diagnosticLabel(input.ReasoningEffort),
		ClientUserAgent: serviceErrorSafeText(ctx, input.ClientUserAgent, 512),
	}
	if ctx.Request != nil {
		if info.ClientUserAgent == "" {
			info.ClientUserAgent = serviceErrorSafeText(ctx, ctx.GetHeader("User-Agent"), 512)
		}
		info.Method = ctx.Request.Method
		info.Endpoint = serviceErrorSafeText(ctx, firstNonEmptyString(ctx.FullPath(), input.InboundEndpoint, ctx.Request.URL.Path), 256)
		info.Transport = "http"
		if isResponsesWebSocketUpgradeRequest(ctx.Request) {
			info.Transport = "websocket"
		}
		if userAgent, known := upstreamUserAgentAudit(ctx.Request.Context()); known {
			info.UpstreamUserAgent = serviceErrorSafeText(ctx, userAgent, 512)
			overridden := normalizeUsageLogUserAgent(ctx.GetHeader("User-Agent")) != userAgent
			info.UserAgentOverridden = &overridden
		}
	}
	return info
}

func usageDiagnosticClientInfo(incoming map[string]map[string]string) map[string]string {
	result := make(map[string]string)
	sources := make([]string, 0, len(incoming))
	for source := range incoming {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	remaining := 4096
	for _, source := range sources {
		fields := diagnosticDeviceFields
		if source == "headers" {
			fields = diagnosticClientHeaders
		}
		for _, field := range fields {
			value := incoming[source][field]
			if value == "" {
				continue
			}
			key := source + "." + field
			if len(result) == 24 || len(key)+len(value) > remaining {
				return result
			}
			result[key] = value
			remaining -= len(key) + len(value)
		}
	}
	return result
}
