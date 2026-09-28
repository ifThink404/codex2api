package proxy

import (
	"encoding/json"

	"github.com/codex2api/auth"
)

const bpsCallerUsagePolicy = "fixed_runtime_overhead_v1"

// Local billing allowances measured with input="1", no caller tools, the current
// developer adaptation, and gpt-6-astra / low on 2026-09-24. Reserve one input
// token for the probe itself. This is a fixed pricing policy, not an exact
// tokenizer or a reduction of actual upstream usage/quota. Recalibrate after
// changing the runtime prompt or upstream prompt/tools.
func bpsFixedInputOverhead(profile auth.CodexBPSProfile) int64 {
	switch auth.NormalizeCodexBPSProfile(string(profile)) {
	case auth.BPSExcel:
		return 22948
	case auth.BPSSheets:
		return 18181
	case auth.BPSPowerPoint:
		return 37679
	default:
		return 13920
	}
}

type bpsUsageDiagnostic struct {
	Policy             string `json:"policy"`
	BaselineSource     string `json:"baseline_source"`
	BaselineOverhead   int64  `json:"baseline_overhead_tokens"`
	UpstreamInput      int64  `json:"upstream_input_tokens"`
	UpstreamCached     int64  `json:"upstream_cached_tokens"`
	UpstreamCacheWrite int64  `json:"upstream_cache_write_tokens"`
	UpstreamOutput     int64  `json:"upstream_output_tokens"`
	ExcludedInput      int64  `json:"excluded_input_tokens"`
	BilledInput        int64  `json:"billed_input_tokens"`
	BilledCached       int64  `json:"billed_cached_tokens"`
	BilledCacheWrite   int64  `json:"billed_cache_write_tokens"`
}

func bpsUsageCount(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	count, err := number.Int64()
	return count, err == nil && count >= 0 && count <= 1<<40
}

func bpsOptionalUsageCount(details map[string]any, key string) (int64, bool) {
	value, exists := details[key]
	if !exists || value == nil {
		return 0, true
	}
	return bpsUsageCount(value)
}

func projectBPSUsage(usage map[string]any, d *CodexBPSDiagnostic) {
	if usage == nil || d == nil || d.Compact || usage["billing_source"] == bpsCallerUsagePolicy {
		return
	}
	input, inputOK := bpsUsageCount(usage["input_tokens"])
	output, outputOK := bpsUsageCount(usage["output_tokens"])
	details, _ := usage["input_tokens_details"].(map[string]any)
	cached, cacheOK := bpsOptionalUsageCount(details, "cached_tokens")
	cacheWrite, writeOK := bpsOptionalUsageCount(details, "cache_write_tokens")
	// Missing/invalid usage cannot be repaired into a charge.
	if !inputOK || !outputOK || !cacheOK || !writeOK {
		return
	}
	overhead := bpsFixedInputOverhead(d.Profile)
	billed := max(int64(0), input-overhead)
	boundedCache := min(cached, input)
	billedCache := min(billed, max(int64(0), boundedCache-overhead))
	// Cached reads and new cache writes are disjoint parts of the input. Remove
	// the fixed prefix from reads first, then writes; never subtract it twice.
	writeOverhead := max(int64(0), overhead-boundedCache)
	billedWrite := min(billed-billedCache, max(int64(0), cacheWrite-writeOverhead))
	d.Usage = &bpsUsageDiagnostic{
		Policy: bpsCallerUsagePolicy, BaselineSource: "input_1_gpt6_astra_low_20260924",
		BaselineOverhead: overhead, UpstreamInput: input, UpstreamCached: cached,
		UpstreamCacheWrite: cacheWrite, UpstreamOutput: output, ExcludedInput: input - billed,
		BilledInput: billed, BilledCached: billedCache, BilledCacheWrite: billedWrite,
	}
	usage["input_tokens"], usage["total_tokens"] = billed, billed+output
	usage["billing_source"], usage["input_tokens_estimated"] = bpsCallerUsagePolicy, true
	if details == nil {
		details = make(map[string]any)
		usage["input_tokens_details"] = details
	}
	details["cached_tokens"] = billedCache
	if _, exists := details["cache_write_tokens"]; exists {
		details["cache_write_tokens"] = billedWrite
	}
	// Fixed overhead is text. Preserve caller image counters while keeping the
	// public breakdown within the adjusted input/cache totals.
	imageInput, _ := bpsUsageCount(details["image_tokens"])
	imageInput = min(imageInput, billed)
	if _, exists := details["image_tokens"]; exists {
		details["image_tokens"] = imageInput
	}
	if _, exists := details["text_tokens"]; exists {
		details["text_tokens"] = max(int64(0), billed-imageInput)
	}
	if cacheDetails, ok := details["cached_tokens_details"].(map[string]any); ok {
		imageCached, _ := bpsUsageCount(cacheDetails["image_tokens"])
		imageCached = min(imageCached, billedCache, imageInput)
		if _, exists := cacheDetails["image_tokens"]; exists {
			cacheDetails["image_tokens"] = imageCached
		}
		if _, exists := cacheDetails["text_tokens"]; exists {
			cacheDetails["text_tokens"] = billedCache - imageCached
		}
	}
}
