package proxy

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func bpsRedactionTestStream(deltas []string, kind string) string {
	var b strings.Builder
	b.WriteString(`data: {"type":"response.created","response":{"id":"resp_r","object":"response"}}` + "\n\n")
	for i, d := range deltas {
		frame := `{"type":"` + kind + `","item_id":"msg_1","output_index":0,"content_index":0,"sequence_number":` + strconv.Itoa(i) + `,"delta":` + jsonString(d) + `}`
		b.WriteString("data: " + frame + "\n\n")
	}
	b.WriteString(`data: {"type":"response.completed","response":{"id":"resp_r","object":"response","output":[]}}` + "\n\n")
	return b.String()
}

func jsonString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func bpsRedactionDeltas(t *testing.T, out []byte) []string {
	t.Helper()
	var deltas []string
	for _, line := range strings.Split(string(out), "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			if d := gjson.Get(data, "delta"); d.Exists() {
				deltas = append(deltas, d.String())
			}
		}
	}
	return deltas
}

func TestBPSStreamRedactsSourceTextSplitAcrossDeltas(t *testing.T) {
	for name, deltas := range map[string][]string{
		"word split":      {"I am Basis Po", "ints, running at bps.open", "ai.com/basispoints/api now."},
		"char by char":    strings.Split("Hello from Basis Points!", ""),
		"escaped host":    {"see bps\\u002eopen", "ai.com"},
		"multibyte after": {"Basis", " Points — 你好"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := bpsProjectionContext(t)
			out, err := bpsTransformTestBody(ctx, bpsRedactionTestStream(deltas, "response.output_text.delta"), true)
			require.NoError(t, err)
			got := bpsRedactionDeltas(t, out)
			require.Len(t, got, len(deltas), "frames are repartitioned, never merged or dropped")
			joined := strings.Join(got, "")
			for _, leak := range []string{"Basis Points", "bps.openai.com", "basispoints", "bps\\u002eopenai"} {
				require.NotContains(t, joined, leak)
			}
			require.NotContains(t, string(out), "Basis Po")
			require.Contains(t, string(out), "response.completed")
			if name == "multibyte after" {
				require.Contains(t, joined, "你好")
			}
		})
	}
}

func TestBPSStreamToolArgumentsKeepBusinessWordingButHideHost(t *testing.T) {
	ctx := bpsProjectionContext(t)
	out, err := bpsTransformTestBody(ctx, bpsRedactionTestStream([]string{`{"topic":"Basis Po`, `ints","url":"https://bps.open`, `ai.com/x"}`}, "response.function_call_arguments.delta"), true)
	require.NoError(t, err)
	joined := strings.Join(bpsRedactionDeltas(t, out), "")
	require.Contains(t, joined, "Basis Points", "caller tool data keeps its wording")
	require.NotContains(t, joined, "bps.openai.com")
}

func TestBPSStreamHoldsOnlyWhileAPrefixIsOpen(t *testing.T) {
	r := newBPSStreamRedactor()
	frames, err := r.push("", []byte(`{"type":"response.output_text.delta","item_id":"m","delta":"plain text "}`))
	require.NoError(t, err)
	require.Len(t, frames, 1, "no protected prefix: released immediately")
	frames, err = r.push("", []byte(`{"type":"response.output_text.delta","item_id":"m","delta":"then Basis"}`))
	require.NoError(t, err)
	require.Empty(t, frames, "an open prefix holds the frame")
	frames, err = r.finish()
	require.NoError(t, err)
	require.Len(t, frames, 1, "stream end releases held frames")
}
