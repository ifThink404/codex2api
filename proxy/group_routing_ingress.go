package proxy

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const groupRoutingIngressKey = "group_routing_ingress"

type groupRoutingIngress struct {
	sessionProvided bool
	effortProvided  bool
}

// Capture HTTP input only once: payload rules, protocol conversion, generated
// identities and the default low effort must not turn absent client fields
// into supplied fields. Raw relay calls this with its decoded inspection copy.
func captureGroupRoutingIngress(c *gin.Context, body []byte) {
	if c == nil {
		return
	}
	if _, exists := c.Get(groupRoutingIngressKey); !exists {
		setGroupRoutingIngress(c, body)
	}
}

// WebSocket callers replace this snapshot for each response.create frame.
func setGroupRoutingIngress(c *gin.Context, body []byte) {
	if c == nil || c.Request == nil || !gjson.ValidBytes(body) {
		return
	}
	input := groupRoutingIngress{}
	headers := CodexRequestMetadataHeaders(c.Request.Header, body)
	// Inspect declared conversation/thread identifiers, not generated affinity,
	// request IDs, task IDs or prompt-cache keys. Metadata projection respects
	// an empty frame-local snapshot instead of reviving stale handshake fields.
	for _, key := range []string{"Session-Id", "Session_id", "Conversation-Id", "Conversation_id", "X-Session-ID", "OpenAI-Session-ID", "X-Session-Affinity", "Thread-Id"} {
		if strings.TrimSpace(headers.Get(key)) != "" {
			input.sessionProvided = true
			break
		}
	}
	turnMetadata := gjson.Parse(headers.Get(codexTurnMetadataHeader))
	for _, path := range []string{"session_id", "thread_id"} {
		value := turnMetadata.Get(path)
		if value.Type == gjson.String && strings.TrimSpace(value.String()) != "" {
			input.sessionProvided = true
		}
	}
	for _, path := range []string{"reasoning.effort", "reasoning_effort"} {
		value := gjson.GetBytes(body, path)
		if value.Type == gjson.String && strings.TrimSpace(value.String()) != "" {
			input.effortProvided = true
			break
		}
	}
	c.Set(groupRoutingIngressKey, input)
}

func missingGroupRoutingInputReason(c *gin.Context) string {
	if c == nil {
		return ""
	}
	value, exists := c.Get(groupRoutingIngressKey)
	input, ok := value.(groupRoutingIngress)
	if !exists || !ok {
		return ""
	}
	switch {
	case !input.sessionProvided && !input.effortProvided:
		return "missing_session_and_reasoning_effort"
	case !input.sessionProvided:
		return "missing_session_id"
	case !input.effortProvided:
		return "missing_reasoning_effort"
	default:
		return ""
	}
}
