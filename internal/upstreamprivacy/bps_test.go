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
		if got == value {
			t.Fatalf("not masked: %q => %q", value, got)
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

func TestResponsesDisplayAddress(t *testing.T) {
	want := "https://chatgpt.com/backend-api/codex/responses"
	for _, source := range []string{"https://" + host + "/basispoints/api/responses", "https://hidden.invalid/basispoints/api/responses"} {
		if got := Text(source); got != want {
			t.Fatalf("address = %q, want %q", got, want)
		}
		if got := Text(source + "/compact"); got != want+"/compact" {
			t.Fatalf("compact address = %q", got)
		}
		encoded, _ := json.Marshal(map[string]any{"url": source})
		encoded = []byte(strings.ReplaceAll(string(encoded), "/", `\/`))
		var decoded map[string]string
		if err := json.Unmarshal(Bytes(encoded), &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded["url"] != want {
			t.Fatalf("escaped address = %q", decoded["url"])
		}
	}
}
