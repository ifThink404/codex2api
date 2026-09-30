package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func bpsTestPNG(t *testing.T) string {
	t.Helper()
	var data bytes.Buffer
	require.NoError(t, png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	return base64.StdEncoding.EncodeToString(data.Bytes())
}

func TestBPSImageMIMENormalizationPreservesProtocol(t *testing.T) {
	encoded := bpsTestPNG(t)
	for _, compact := range []bool{false, true} {
		for _, kind := range []string{"message", "function_call_output", "custom_tool_call_output"} {
			t.Run(fmt.Sprintf("%s/compact=%t", kind, compact), func(t *testing.T) {
				field := "output"
				item := map[string]any{"type": kind, "call_id": "call_original", "id": "item_original", "large_integer": json.Number("9007199254740993")}
				if kind == "message" {
					field, item["role"] = "content", "user"
				}
				item[field] = []any{
					map[string]any{"type": "input_text", "text": "Keep this text and image."},
					map[string]any{"type": "input_image", "image_url": "data:application/octet-stream;base64," + encoded, "detail": "original"},
				}
				body, err := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": []any{item}})
				require.NoError(t, err)
				before := append([]byte(nil), body...)
				out, d, err := prepareCodexBPSBody(body, "cache", compact)
				require.NoError(t, err)
				require.Equal(t, before, body)
				require.Equal(t, "data:image/png;base64,"+encoded, gjson.GetBytes(out, "input.1."+field+".1.image_url").String())
				require.Equal(t, "high", gjson.GetBytes(out, "input.1."+field+".1.detail").String())
				require.Equal(t, "call_original", gjson.GetBytes(out, "input.1.call_id").String())
				require.Equal(t, "item_original", gjson.GetBytes(out, "input.1.id").String())
				require.Equal(t, "9007199254740993", gjson.GetBytes(out, "input.1.large_integer").Raw)
				require.Equal(t, "Keep this text and image.", gjson.GetBytes(out, "input.1."+field+".0.text").String())
				require.Equal(t, 1, d.Images.MIMENormalized)
				require.Equal(t, "input[1]."+field+"[1]", d.Images.Details[0].Path)
				logged, err := json.Marshal(d)
				require.NoError(t, err)
				require.NotContains(t, string(logged), encoded)
				require.NotContains(t, string(logged), "call_original")
				require.NotContains(t, string(out), "mime_normalized")
				items := gjson.GetBytes(out, "input").Array()
				raw := make([]json.RawMessage, len(items))
				for i, v := range items {
					raw[i] = json.RawMessage(v.Raw)
				}
				_, again := normalizeBPSInputImages(raw)
				require.Zero(t, again.MIMENormalized)
			})
		}
	}
}

func TestBPSImageNormalizationDoesNotRewriteBusinessContainers(t *testing.T) {
	imageURL := "data:application/octet-stream;base64," + bpsTestPNG(t)
	imageObject := map[string]any{"type": "input_image", "image_url": imageURL}
	business, err := json.Marshal(imageObject)
	require.NoError(t, err)
	input := []any{
		map[string]any{"type": "function_call", "call_id": "call_1", "arguments": string(business)},
		map[string]any{"type": "function_call_output", "call_id": "call_1", "output": string(business)},
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": string(business)}}},
		map[string]any{"type": "additional_tools", "tools": []any{map[string]any{"type": "function", "name": "test", "parameters": imageObject}}},
		map[string]any{"type": "reasoning", "encrypted_content": string(business)},
	}
	for _, v := range input {
		raw, err := json.Marshal(v)
		require.NoError(t, err)
		out, d := normalizeBPSInputImages([]json.RawMessage{raw})
		require.Nil(t, d)
		require.Equal(t, raw, []byte(out[0]))
	}
}

func TestBPSImageReferencesAndBoundedDiagnostics(t *testing.T) {
	encoded := bpsTestPNG(t)
	cases := []struct{ value, expected, action string }{
		{"data:image/jpeg;base64," + encoded, "data:image/png;base64," + encoded, "mime_normalized"},
		{"DATA:application/octet-stream;BASE64," + encoded, "data:image/png;base64," + encoded, "mime_normalized"},
		{"data:image/png;base64," + encoded, "data:image/png;base64," + encoded, "preserved"},
		{"https://image.invalid/private?token=SECRET", "https://image.invalid/private?token=SECRET", "preserved"},
		{"data:application/octet-stream;base64,!!!", "data:application/octet-stream;base64,!!!", "invalid_base64"},
		{"data:text/plain;base64,aGVsbG8=", "data:text/plain;base64,aGVsbG8=", "unknown_image_format"},
		{"data:image/png,not-base64", "data:image/png,not-base64", "unsupported_data_encoding"},
	}
	for _, scenario := range cases {
		raw, err := json.Marshal(map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": scenario.value}}})
		require.NoError(t, err)
		out, d := normalizeBPSInputImages([]json.RawMessage{raw})
		require.Equal(t, scenario.expected, gjson.GetBytes(out[0], "content.0.image_url").String())
		require.Equal(t, scenario.action, d.Details[0].Action)
		logged, err := json.Marshal(d)
		require.NoError(t, err)
		require.NotContains(t, string(logged), "SECRET")
	}
	var input []json.RawMessage
	for i := 0; i < 20; i++ {
		input = append(input, json.RawMessage(`{"role":"user","content":[{"type":"input_image","file_id":"private-file-id"}]}`))
	}
	_, d := normalizeBPSInputImages(input)
	require.Equal(t, 20, d.Count)
	require.Equal(t, 12, d.DetailsOmitted)
	require.Len(t, d.Details, 8)
	require.Equal(t, "input[19].content[0]", d.Details[7].Path)
	logged, err := json.Marshal(d)
	require.NoError(t, err)
	require.False(t, strings.Contains(string(logged), "private-file-id"))
}
