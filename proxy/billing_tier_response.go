package proxy

import (
	"context"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type billingTierRequestKey struct{}

type billingTierResponse struct {
	Version                 int    `json:"version"`
	ServiceTier             string `json:"service_tier"`
	Source                  string `json:"source"`
	RequestedServiceTier    string `json:"requested_service_tier"`
	ActualServiceTier       string `json:"actual_service_tier"`
	LocalBillingServiceTier string `json:"local_billing_service_tier"`
}

func knownBillingTier(value string) string {
	switch value = normalizeBillingServiceTier(value); value {
	case "priority", "ultrafast", "default", "standard", "auto", "flex", "scale":
		return value
	default:
		return ""
	}
}

// Report the execution tier separately from the gateway's local pricing policy.
// The downstream may use the execution tier as a fallback without changing the
// client's original request or pretending it explicitly requested Priority.
func projectBillingTierResponse(ctx context.Context, body []byte) []byte {
	requested, enabled := ctx.Value(billingTierRequestKey{}).(string)
	if !enabled || !gjson.ValidBytes(body) {
		return body
	}
	root := gjson.ParseBytes(body)
	response, path := root, "codex2api_billing"
	if strings.HasPrefix(root.Get("type").String(), "response.") && root.Get("response").IsObject() {
		response, path = root.Get("response"), "response.codex2api_billing"
	} else if root.Get("type").Exists() || !(root.Get("object").String() == "response" || root.Get("object").String() == "response.compaction" || root.Get("output").IsArray()) {
		return body
	}
	actual := knownBillingTier(response.Get("service_tier").String())
	tier, source := actual, "upstream_response"
	if tier == "" {
		tier, source = requested, "effective_request"
	}
	if tier == "" {
		source = "unobserved"
	}
	local := resolveBillingServiceTier(actual, requested)
	if local == "" {
		local = "default"
	}
	value := billingTierResponse{Version: 1, ServiceTier: tier, Source: source, RequestedServiceTier: requested, ActualServiceTier: actual, LocalBillingServiceTier: local}
	encoded, err := sjson.SetBytes(body, path, value)
	if err != nil {
		return body
	}
	return encoded
}
