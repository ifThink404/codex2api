package admin

import (
	"encoding/json"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/tidwall/sjson"
)

func buildCodexIndependentTestPayload(account *auth.Account, model, content, sessionID string) []byte {
	payload := buildTestPayloadWithContent(model, content)
	payload, _ = sjson.SetBytes(payload, "instructions", "")
	payload, _ = sjson.SetBytes(payload, "reasoning.effort", "medium")
	threadID := proxy.NewUpstreamSessionUUID()
	turnID := proxy.NewUpstreamSessionUUID()
	parentTurnID := proxy.NewUpstreamSessionUUID()
	contextWindowID := proxy.NewUpstreamSessionUUID()
	windowID := threadID + ":0"
	// Connection tests have no incoming client metadata. Explicitly model the
	// official base URL in both locations; the common outbound pass derives the
	// HTTP compatibility header from this same turn metadata.
	baseURL := proxy.CodexBaseURL
	turnMetadata := map[string]any{
		"base_url":                baseURL,
		"openai_base_url":         baseURL,
		"request_kind":            "turn",
		"thread_source":           "subagent",
		"subagent_kind":           "thread_spawn",
		"session_id":              sessionID,
		"thread_id":               threadID,
		"parent_thread_id":        sessionID,
		"turn_id":                 turnID,
		"parent_turn_id":          parentTurnID,
		"root_turn_id":            parentTurnID,
		"context_window_id":       contextWindowID,
		"window_id":               windowID,
		"window_number":           0,
		"turn_started_at_unix_ms": time.Now().UnixMilli(),
	}
	clientMetadata := map[string]any{
		"base_url":                 baseURL,
		"openai_base_url":          baseURL,
		"session_id":               sessionID,
		"thread_id":                threadID,
		"turn_id":                  turnID,
		"parent_turn_id":           parentTurnID,
		"root_turn_id":             parentTurnID,
		"x-codex-parent-thread-id": sessionID,
		"x-openai-subagent":        "thread_spawn",
		"x-codex-window-id":        windowID,
		"x-client-request-id":      threadID,
	}
	if account != nil {
		if installationID := account.EffectiveCodexInstallationID(); installationID != "" {
			turnMetadata["installation_id"] = installationID
			clientMetadata["x-codex-installation-id"] = installationID
		}
	}
	encodedMetadata, _ := json.Marshal(turnMetadata)
	clientMetadata["x-codex-turn-metadata"] = string(encodedMetadata)
	payload, _ = sjson.SetBytes(payload, "client_metadata", clientMetadata)
	payload, _ = sjson.SetBytes(payload, "prompt_cache_key", sessionID)
	return payload
}
