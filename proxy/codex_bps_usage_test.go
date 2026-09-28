package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestBPSFixedUsageMeasuredProfiles(t *testing.T) {
	for _, tc := range []struct {
		profile                auth.CodexBPSProfile
		input, cache, overhead int64
	}{
		{auth.BPSWord, 13921, 13853, 13920}, {auth.BPSExcel, 22949, 22881, 22948},
		{auth.BPSSheets, 18182, 18114, 18181}, {auth.BPSPowerPoint, 37680, 37612, 37679},
	} {
		for _, warm := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/warm=%v", tc.profile, warm), func(t *testing.T) {
				read, write := int64(0), tc.cache
				if warm {
					read, write = tc.cache, 0
				}
				d := &CodexBPSDiagnostic{Mode: "bps", Profile: tc.profile}
				ctx := context.WithValue(t.Context(), codexBPSDiagnosticKey{}, d)
				raw := []byte(fmt.Sprintf(`{"type":"response.completed","response":{"object":"response","usage":{"input_tokens":%d,"input_tokens_details":{"cached_tokens":%d,"cache_write_tokens":%d},"output_tokens":15,"total_tokens":%d}}}`, tc.input, read, write, tc.input+15))
				out, err := projectBPSResponse(ctx, raw)
				require.NoError(t, err)
				usage := extractUsage(out)
				require.NotNil(t, usage)
				require.Equal(t, 1, usage.InputTokens)
				require.Zero(t, usage.CachedTokens)
				require.Equal(t, 15, usage.OutputTokens)
				require.Zero(t, gjson.GetBytes(out, "response.usage.input_tokens_details.cache_write_tokens").Int())
				require.EqualValues(t, 16, gjson.GetBytes(out, "response.usage.total_tokens").Int())
				require.Equal(t, tc.overhead, d.Usage.BaselineOverhead)
				require.Equal(t, tc.input, d.Usage.UpstreamInput)
				require.Equal(t, read, d.Usage.UpstreamCached)
				require.Equal(t, write, d.Usage.UpstreamCacheWrite)
			})
		}
	}
}

func TestBPSCallerUsageProjectionAndBilling(t *testing.T) {
	for _, tc := range []struct {
		name                                                   string
		input, cached, write, billed, billedCache, billedWrite int
	}{
		{"cold", 14020, 0, 13920, 100, 0, 0},
		{"runtime_cached_only", 14020, 13920, 0, 100, 0, 0},
		{"caller_partly_cached", 14020, 13960, 0, 100, 40, 0},
		{"caller_fully_cached", 14020, 14020, 0, 100, 100, 0},
		{"mixed_read_write", 14020, 13900, 120, 100, 0, 100},
		{"caller_read_and_write", 14020, 13960, 60, 100, 40, 60},
		{"shorter_than_allowance", 50, 20, 30, 0, 0, 0},
		{"explicit_zero", 0, 0, 0, 0, 0, 0},
		{"cache_bounded_to_input", 14020, 15000, 10000, 100, 100, 0},
		{"later_history_is_retained", 34020, 33920, 0, 20100, 20000, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &CodexBPSDiagnostic{Mode: "bps", Profile: auth.BPSWord, projection: newBPSResponseProjection([]byte(`{"input":"hi"}`))}
			ctx := context.WithValue(t.Context(), codexBPSDiagnosticKey{}, d)
			raw := []byte(fmt.Sprintf(`{"type":"response.completed","response":{"object":"response","output":[],"usage":{"input_tokens":%d,"input_tokens_details":{"cached_tokens":%d,"cache_write_tokens":%d},"output_tokens":20,"output_tokens_details":{"reasoning_tokens":7},"total_tokens":%d}}}`, tc.input, tc.cached, tc.write, tc.input+20))
			out, err := projectBPSResponse(ctx, raw)
			require.NoError(t, err)
			usage := extractUsage(out)
			require.NotNil(t, usage)
			require.Equal(t, tc.billed, usage.InputTokens)
			require.Equal(t, tc.billedCache, usage.CachedTokens)
			require.EqualValues(t, tc.billedWrite, gjson.GetBytes(out, "response.usage.input_tokens_details.cache_write_tokens").Int())
			require.Equal(t, 20, usage.OutputTokens)
			require.Equal(t, 7, usage.ReasoningTokens)
			require.EqualValues(t, tc.billed+20, gjson.GetBytes(out, "response.usage.total_tokens").Int())
			require.Equal(t, bpsCallerUsagePolicy, gjson.GetBytes(out, "response.usage.billing_source").String())
			require.True(t, gjson.GetBytes(out, "response.usage.input_tokens_estimated").Bool())
			require.EqualValues(t, tc.input, d.Usage.UpstreamInput)
			require.EqualValues(t, tc.cached, d.Usage.UpstreamCached)
			cost := database.CalculateCostBreakdown(usage.InputTokens, usage.OutputTokens, usage.CachedTokens, "gpt-6-astra", "")
			require.InDelta(t, float64(tc.billed-tc.billedCache)*cost.InputPricePerMToken/1e6, cost.InputCost, 1e-12)
			require.InDelta(t, float64(tc.billedCache)*cost.CacheReadPricePerMToken/1e6, cost.CacheReadCost, 1e-12)
			require.InDelta(t, 20*cost.OutputPricePerMToken/1e6, cost.OutputCost, 1e-12)
			firstDiagnostic, _ := json.Marshal(d.Usage)
			again, err := projectBPSResponse(ctx, out)
			require.NoError(t, err)
			require.JSONEq(t, string(out), string(again))
			secondDiagnostic, _ := json.Marshal(d.Usage)
			require.Equal(t, firstDiagnostic, secondDiagnostic)
		})
	}
}

func TestBPSUsagePreservesCallerImageUsage(t *testing.T) {
	ctx := context.WithValue(t.Context(), codexBPSDiagnosticKey{}, &CodexBPSDiagnostic{Profile: auth.BPSWord})
	raw := []byte(`{"object":"response","usage":{"input_tokens":16920,"output_tokens":25,"input_tokens_details":{"cached_tokens":15920,"image_tokens":1000,"text_tokens":15920,"cached_tokens_details":{"image_tokens":1000,"text_tokens":14920}}}}`)
	out, err := projectBPSResponse(ctx, raw)
	require.NoError(t, err)
	for path, want := range map[string]int64{"input_tokens": 3000, "output_tokens": 25, "total_tokens": 3025, "input_tokens_details.image_tokens": 1000, "input_tokens_details.text_tokens": 2000, "input_tokens_details.cached_tokens": 2000, "input_tokens_details.cached_tokens_details.image_tokens": 1000, "input_tokens_details.cached_tokens_details.text_tokens": 1000} {
		require.Equal(t, want, gjson.GetBytes(out, "usage."+path).Int(), path)
	}
}

func TestBPSUsageDoesNotFabricateMissingUsageOrChangeOtherProviders(t *testing.T) {
	ctx := bpsProjectionContext(t)
	for _, payload := range []string{
		`{"object":"response","output":[]}`, `{"object":"response","usage":null}`,
		`{"object":"response","usage":{"output_tokens":10}}`,
		`{"object":"response","usage":{"input_tokens":-1,"output_tokens":10}}`,
		`{"object":"response","usage":{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cached_tokens":-1}}}`,
		`{"object":"response","usage":{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cache_write_tokens":-1}}}`,
	} {
		out, err := projectBPSResponse(ctx, []byte(payload))
		require.NoError(t, err)
		require.JSONEq(t, payload, string(out))
	}
	plain := []byte(`{"object":"response","usage":{"input_tokens":20100,"output_tokens":20,"input_tokens_details":{"cached_tokens":20000}}}`)
	unchanged, err := projectBPSResponse(t.Context(), plain)
	require.NoError(t, err)
	require.True(t, bytes.Equal(plain, unchanged))
	compact := context.WithValue(t.Context(), codexBPSDiagnosticKey{}, &CodexBPSDiagnostic{Compact: true})
	unchanged, err = projectBPSResponse(compact, plain)
	require.NoError(t, err)
	require.JSONEq(t, string(plain), string(unchanged))
	business := []byte(`{"type":"response.output_item.done","item":{"type":"function_call","name":"echo","call_id":"c","arguments":"{\"usage\":{\"input_tokens\":20100,\"output_tokens\":20}}"}}`)
	unchanged, err = projectBPSResponse(ctx, business)
	require.NoError(t, err)
	require.JSONEq(t, string(business), string(unchanged))
}

func TestBPSCallerBillingThroughHTTPExecutor(t *testing.T) {
	for _, profile := range []auth.CodexBPSProfile{auth.BPSWord, auth.BPSExcel, auth.BPSSheets, auth.BPSPowerPoint} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", profile, streaming), func(t *testing.T) {
				a := withBPSOverride((&auth.Account{DBID: 1695, AccountID: accountIdentitySampleAccount, AccessToken: "test"}).SetCodexBPSOptions(auth.CodexBPSAccountOptions{Profile: profile}), true)
				caller := []byte(`{"model":"gpt-6-astra","input":"Reply hello","tools":[{"type":"function","name":"echo","parameters":{"type":"object"}}]}`)
				overhead := bpsFixedInputOverhead(profile)
				installClaudeBoundaryTransport(t, a, func(r *http.Request) (*http.Response, error) {
					wire, err := io.ReadAll(r.Body)
					require.NoError(t, err)
					require.Contains(t, string(wire), "RUNTIME OVERRIDE")
					require.Contains(t, string(wire), "echo")
					payload := fmt.Sprintf(`{"object":"response","id":"resp_billing","output":[],"usage":{"input_tokens":%d,"output_tokens":20,"total_tokens":%d,"input_tokens_details":{"cached_tokens":%d}}}`, overhead+100, overhead+120, overhead+40)
					header := http.Header{"Content-Type": []string{"application/json"}}
					if streaming {
						payload = "data: {\"type\":\"response.completed\",\"response\":" + payload + "}\n\n"
						header.Set("Content-Type", "text/event-stream")
					}
					return &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(strings.NewReader(payload)), Request: r}, nil
				})
				response, err := executeBPSTestRequest(t.Context(), a, caller, "billing-test", "", "test-key", nil, nil, true)
				require.NoError(t, err)
				defer response.Body.Close()
				// Configuration changes after dispatch cannot change this attempt's allowance.
				a.SetCodexBPSOptions(auth.CodexBPSAccountOptions{Profile: auth.BPSWord, Convergence: a.CodexBPSConvergence(), ImageTrim: a.CodexBPSImageTrimEnabled()})
				out, err := io.ReadAll(response.Body)
				require.NoError(t, err)
				path := "usage"
				if streaming {
					out = bytes.TrimSpace(bytes.TrimPrefix(out, []byte("data: ")))
					path = "response.usage"
				}
				usage := extractUsageFromResult(gjson.GetBytes(out, path))
				require.NotNil(t, usage, string(out))
				require.Equal(t, 100, usage.InputTokens)
				require.Equal(t, 40, usage.CachedTokens)
				require.Equal(t, 20, usage.OutputTokens)
				d := CodexBPSResponseDiagnostic(response)
				require.NotNil(t, d.Usage)
				require.EqualValues(t, overhead+100, d.Usage.UpstreamInput)
			})
		}
	}
}
