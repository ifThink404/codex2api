package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func imageRewriteFixture(t testing.TB, count, size int) ([]byte, map[int][]bpsImageReference) {
	t.Helper()
	items := make([]json.RawMessage, 0, count+1)
	items = append(items, json.RawMessage(`{"type":"function_call","namespace":"functions","name":"read","call_id":"call1","arguments":"{\"image_url\":\"business\"}","large_integer":9007199254740993}`))
	refs := make(map[int][]bpsImageReference)
	for i := 0; i < count; i++ {
		items = append(items, json.RawMessage(fmt.Sprintf(`{"type":"function_call_output","call_id":"call%d","output":[{"type":"input_image","image_url":"data:image/png;base64,%s","detail":"original","extra":9007199254740993}]}`, i, strings.Repeat("x", size))))
		refs[i+1] = []bpsImageReference{{Field: "output", Part: 0, ID: fmt.Sprintf("file-%d", i)}}
	}
	encoded, err := json.Marshal(items)
	require.NoError(t, err)
	return []byte(`{"input":` + string(encoded) + `,"metadata":{"large_integer":9007199254740993}}`), refs
}

func TestBPSBatchImageRewritePreservesOpaqueHistory(t *testing.T) {
	body, refs := imageRewriteFixture(t, 68, 1024)
	wire, err := rewriteBPSImageReferences(body, refs)
	require.NoError(t, err)
	items := gjson.GetBytes(wire, "input").Array()
	require.Len(t, items, 69)
	require.Equal(t, gjson.GetBytes(body, "input.0").Raw, items[0].Raw)
	require.Equal(t, "9007199254740993", gjson.GetBytes(wire, "metadata.large_integer").Raw)
	for i, item := range items[1:] {
		require.Equal(t, fmt.Sprintf("call%d", i), item.Get("call_id").String())
		require.Equal(t, fmt.Sprintf("file-%d", i), item.Get("output.0.file_id").String())
		require.False(t, item.Get("output.0.image_url").Exists())
		require.Equal(t, "original", item.Get("output.0.detail").String())
		require.Equal(t, "9007199254740993", item.Get("output.0.extra").Raw)
	}
	require.Contains(t, string(body), "data:image/png;base64,")
}

func BenchmarkBPSImageReferenceRewrite(b *testing.B) {
	body, refs := imageRewriteFixture(b, 68, 1024*1024)
	for _, batch := range []bool{false, true} {
		name := "previous_full_body_updates"
		if batch {
			name = "batch"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if batch {
					_, err := rewriteBPSImageReferences(body, refs)
					require.NoError(b, err)
					continue
				}
				wire := body
				for i := 1; i <= len(refs); i++ {
					var err error
					wire, err = sjson.DeleteBytes(wire, fmt.Sprintf("input.%d.output.0.image_url", i))
					require.NoError(b, err)
					wire, err = sjson.SetBytes(wire, fmt.Sprintf("input.%d.output.0.file_id", i), refs[i][0].ID)
					require.NoError(b, err)
				}
			}
		})
	}
}
