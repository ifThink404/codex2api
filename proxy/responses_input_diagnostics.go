package proxy

import (
	"net/http"
	"strings"

	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

type responsesInputDiagnostic struct {
	CompactionMetadata         *compactionMetadataDiagnostic `json:"compaction_metadata,omitempty"`
	Tools                      *database.SessionToolSummary  `json:"tools,omitempty"`
	Mode                       string                        `json:"mode"`
	JSONBytes                  int                           `json:"json_bytes"`
	InputKind                  string                        `json:"input_kind"`
	InputItems                 int                           `json:"input_items"`
	MetadataCompaction         bool                          `json:"metadata_compaction"`
	ProtocolTriggerCount       int                           `json:"protocol_trigger_count"`
	TriggerAtEnd               bool                          `json:"trigger_at_end"`
	CompactionItems            int                           `json:"compaction_items"`
	EncryptedCompactionItems   int                           `json:"encrypted_compaction_items"`
	EncryptedCompactionBytes   int                           `json:"encrypted_compaction_bytes"`
	CompactionItemsWithID      int                           `json:"compaction_items_with_id"`
	PreviousResponseIDPresent  bool                          `json:"previous_response_id_present"`
	SummaryPrefixItems         int                           `json:"summary_prefix_items"`
	ToolOutputPlaceholderItems int                           `json:"tool_output_placeholder_items"`
}

func diagnoseResponsesInput(body []byte, headers http.Header, endpoint string) *responsesInputDiagnostic {
	if !gjson.ValidBytes(body) {
		return nil
	}
	root := gjson.ParseBytes(body)
	input := root.Get("input")
	metadata := diagnosticMetadataObject(root.Get("client_metadata.x-codex-turn-metadata"))
	metadataCompaction := turnMetadataIndicatesCompaction(metadata.Raw) || turnMetadataIndicatesCompaction(headers.Get(codexTurnMetadataHeader))
	compactEndpoint := strings.HasSuffix(strings.TrimRight(endpoint, "/"), "/responses/compact")
	if !input.Exists() && !metadataCompaction && !compactEndpoint {
		return nil
	}
	shape := &responsesInputDiagnostic{
		Tools: summarizeSessionTools(body),
		Mode:  "ordinary", JSONBytes: len(body), InputKind: "absent", MetadataCompaction: metadataCompaction,
		PreviousResponseIDPresent: strings.TrimSpace(root.Get("previous_response_id").String()) != "",
	}
	inspect := func(item gjson.Result) {
		shape.InputItems++
		shape.TriggerAtEnd = isDirectCompactionTrigger(item)
		if shape.TriggerAtEnd {
			shape.ProtocolTriggerCount++
		}
		if isResponsesCompactionItemType(item.Get("type").String()) {
			shape.CompactionItems++
			if item.Get("id").Exists() {
				shape.CompactionItemsWithID++
			}
			if encrypted := item.Get("encrypted_content"); encrypted.Type == gjson.String && encrypted.String() != "" {
				shape.EncryptedCompactionItems++
				shape.EncryptedCompactionBytes += len(encrypted.String())
			}
		}
		if item.Get("role").String() == "developer" {
			content := item.Get("content")
			summaryPrefix := content.Type == gjson.String && strings.HasPrefix(content.String(), responsesCompactionSummaryPrefix)
			if content.IsArray() {
				content.ForEach(func(_, part gjson.Result) bool {
					if part.Get("type").String() == "input_text" && strings.HasPrefix(part.Get("text").String(), responsesCompactionSummaryPrefix) {
						summaryPrefix = true
					}
					return !summaryPrefix
				})
			}
			if summaryPrefix {
				shape.SummaryPrefixItems++
			}
		}
		if item.Get("type").String() == "function_call_output" && item.Get("output").String() == "[tool output was not recorded]" {
			shape.ToolOutputPlaceholderItems++
		}
	}
	switch {
	case input.IsArray():
		shape.InputKind = "array"
		input.ForEach(func(_, item gjson.Result) bool {
			inspect(item)
			return true
		})
	case input.IsObject():
		shape.InputKind = "object"
		inspect(input)
	case input.Type == gjson.String:
		shape.InputKind = "string"
	case input.Exists():
		shape.InputKind = "other"
	}
	switch {
	case shape.ProtocolTriggerCount > 0:
		shape.Mode = "protocol_trigger"
	case compactEndpoint:
		shape.Mode = "compact_endpoint"
	case metadataCompaction:
		shape.Mode = "metadata_only"
	case shape.CompactionItems > 0:
		shape.Mode = "history_only"
	}
	compaction := captureCompactionMetadata(root, headers)
	if metadataCompaction || compactEndpoint || shape.ProtocolTriggerCount > 0 || compaction.Header.State != "absent" || compaction.Body.State != "absent" {
		shape.CompactionMetadata = compaction
	}
	return shape
}

func (observer *TransportObserver) ResponsesInput(body []byte, headers http.Header, endpoint string) {
	if observer == nil {
		return
	}
	shape := diagnoseResponsesInput(body, headers, endpoint)
	toolProtocol := diagnoseToolProtocol(body)
	outbound := captureOutboundIdentityBody(body)
	accessPrograms := captureAccessPrograms(body)
	turnState := captureUsageOutboundTurnState(body, headers)
	observer.update(func(diagnostic *UpstreamTransportDiagnostic) {
		diagnostic.ResponsesInput = shape
		diagnostic.ToolProtocol = toolProtocol
		diagnostic.AccessPrograms = accessPrograms
		diagnostic.RequestTurnState = turnState
	})
	observer.updateOutboundIdentity(func(identity *outboundIdentityDiagnostic) {
		identity.Body = outbound
	})
}
