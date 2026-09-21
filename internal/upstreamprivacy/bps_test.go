package upstreamprivacy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestHostRepresentationsAndPrefix(t *testing.T) {
	values := []string{host, strings.ToUpper(host), `b\u0070s.openai.com`, `bps%2eopenai%252ecom`, `\u0062\u0070\u0073\u002eopenai.com`, strings.ReplaceAll(host, ".", `\\u002e`)}
	for _, value := range values {
		got := Text(value)
		if got == value || len(got) != len(value) {
			t.Fatalf("not masked or length changed: %q => %q", value, got)
		}
		for split := 1; split < len(value); split++ {
			if !Prefix(value[:split]) {
				t.Fatalf("unfinished prefix released: %q", value[:split])
			}
		}
		data, _ := json.Marshal(map[string]any{"nested": []string{value}, "arguments": `{"url":"https://` + value + `/basispoints/api/responses"}`})
		masked := Bytes(data)
		if !json.Valid(masked) || string(masked) == string(data) {
			t.Fatalf("invalid/unmasked JSON: %s", masked)
		}
	}
	for _, v := range []string{"hello", "bpt", "https://example.com", "upstream.local"} {
		if Text(v) != v {
			t.Fatalf("unrelated value changed: %q", v)
		}
	}
}
