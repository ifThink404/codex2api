package proxy

import (
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

// Reasoning effort and agent iteration (adopted from upstream's official Excel
// BPS adapter). BPS accepts low, medium, high and xhigh; other picker values
// map to the nearest tier instead of failing the request. An absent effort
// keeps fj's default (low).

func normalizeBPSReasoningEffort(effort string) string {
	switch value := strings.ToLower(strings.TrimSpace(effort)); value {
	case "low", "medium", "high", "xhigh":
		return value
	case "none", "minimal":
		return "low"
	case "x-high", "extra-high", "extra_high", "max", "ultra":
		return "xhigh"
	default:
		// Unknown or stale picker values fall back to medium.
		return "medium"
	}
}

// bpsEffortLabel bounds a caller-supplied effort for the diagnostic.
func bpsEffortLabel(effort string) string {
	effort = strings.TrimSpace(effort)
	if len(effort) > 32 {
		return effort[:32] + "…"
	}
	return effort
}

// bpsAgentIteration is the fallback agent_iteration when no Word identity or
// convergence scope assigns one: 1 for a user message, plus one per round of
// tool results after it. Parallel results arrive together as one round, as
// the Excel add-in counts them.
func bpsAgentIteration(input gjson.Result) string {
	items := input.Array()
	lastUser := -1
	for i, item := range items {
		if item.Get("role").String() == "user" {
			lastUser = i
		}
	}
	iteration, inResults := 1, false
	for _, item := range items[lastUser+1:] {
		result := strings.HasSuffix(item.Get("type").String(), "_call_output")
		if result && !inResults {
			iteration++
		}
		inResults = result
	}
	return strconv.Itoa(iteration)
}
