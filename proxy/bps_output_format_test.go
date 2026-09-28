package proxy

import (
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func bpsDeveloperTexts(body []byte) []string {
	var texts []string
	for _, item := range gjson.GetBytes(body, "input").Array() {
		if item.Get("role").String() == "developer" {
			texts = append(texts, item.Get("content.0.text").String())
		}
	}
	return texts
}

func TestBPSTextFormatBecomesAnOutputContract(t *testing.T) {
	schema := `{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`
	for _, tc := range []struct {
		name, format string
		want         []string
	}{
		{name: "json object", format: `{"type":"json_object"}`, want: []string{"exactly one valid JSON object"}},
		{name: "json schema", format: `{"type":"json_schema","name":"reply","description":"One answer","schema":` + schema + `,"strict":true}`,
			want: []string{"conforms to the JSON Schema", "Schema name: reply", "Schema description: One answer", `{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"gpt-6-astra","instructions":"Be brief.","input":"hi","text":{"format":` + tc.format + `}}`)
			out, d, err := prepareCodexBPSBodyForProfile(body, "cache", false, false, nil, bpsProfile(auth.BPSExcel))
			require.NoError(t, err)
			require.False(t, gjson.GetBytes(out, "text").Exists(), "the BPS wire body has no text field")
			texts := bpsDeveloperTexts(out)
			contract := texts[len(texts)-1]
			for _, want := range tc.want {
				require.Contains(t, contract, want)
			}
			require.Equal(t, "Be brief.", texts[len(texts)-2], "the contract follows the caller instructions")
			require.Contains(t, d.AdaptedFields, "text.format → input.developer output contract (not enforced)")
		})
	}
}

func TestBPSTextFormatPlainTextAndCompactAddNothing(t *testing.T) {
	plain := []byte(`{"model":"gpt-6-astra","input":"hi","text":{"format":{"type":"text"},"verbosity":"low"}}`)
	out, _, err := prepareCodexBPSBodyForProfile(plain, "cache", false, false, nil, bpsProfile(auth.BPSExcel))
	require.NoError(t, err)
	for _, text := range bpsDeveloperTexts(out) {
		require.NotContains(t, text, "Final answer format")
	}
	compact := []byte(`{"model":"gpt-6-astra","input":"hi","text":{"format":{"type":"json_object"}}}`)
	out, _, err = prepareCodexBPSBodyForProfile(compact, "cache", true, false, nil, bpsProfile(auth.BPSExcel))
	require.NoError(t, err)
	require.NotContains(t, string(out), "Final answer format")
}

func TestBPSTextFormatRejectsUnsupportedFormats(t *testing.T) {
	for _, format := range []string{`{"type":"json_schema"}`, `{"type":"json_schema","schema":"not an object"}`, `{"type":"grammar","definition":"x"}`, `{"type":"` + strings.Repeat("x", 200) + `"}`} {
		body := []byte(`{"model":"gpt-6-astra","input":"hi","text":{"format":` + format + `}}`)
		_, _, err := prepareCodexBPSBodyForProfile(body, "cache", false, false, nil, bpsProfile(auth.BPSExcel))
		var e *Error
		require.ErrorAs(t, err, &e, format)
		require.Equal(t, 400, e.HTTPStatus)
		require.Less(t, len(e.Message), 160)
	}
}
