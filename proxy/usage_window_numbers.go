package proxy

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

// Read the current request body, never a reused WS handshake's window number.
func populateUsageWindowNumbers(snapshot *usageRequestDiagnostics, input *database.UsageLogInput) {
	input.WindowNumberOriginal = snapshot.WindowNumberOriginal
	input.WindowNumberOutbound = ""
	input.BPSAgentIteration = ""
	if snapshot.Upstream == nil || snapshot.Upstream.OutboundIdentity == nil {
		return
	}
	identity := snapshot.Upstream.OutboundIdentity
	var headers http.Header
	if identity.HTTP != nil {
		headers = make(http.Header)
		for name, value := range identity.HTTP.Headers {
			headers.Set(name, value)
		}
	}
	var body []byte
	if identity.Body != nil {
		body, _ = json.Marshal(identity.Body)
	}
	if identity.SessionHeaderPolicy == "bps" {
		value := gjson.GetBytes(body, "metadata.agent_iteration")
		if number, err := strconv.ParseUint(value.String(), 10, 64); err == nil && number > 0 {
			input.BPSAgentIteration = strconv.FormatUint(number, 10)
		}
		return
	}
	if input.WindowNumberOriginal == "" {
		return
	}
	_, number, known, invalid := parseContinuityWindow(headers, body, false)
	if known && invalid == "" {
		input.WindowNumberOutbound = strconv.FormatUint(number, 10)
	}
	snapshot.WindowNumberOutbound = input.WindowNumberOutbound
}
