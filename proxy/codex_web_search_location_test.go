package proxy

import (
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

func withCodexWebSearchLocation(t *testing.T, enabled bool, locations map[string]database.ProxyLocation) {
	t.Helper()
	previous := CodexWebSearchProxyLocationEnabled()
	SetCodexWebSearchProxyLocation(enabled)
	SetCodexProxyLocationResolver(func(proxyURL string) database.ProxyLocation { return locations[proxyURL] })
	t.Cleanup(func() {
		SetCodexWebSearchProxyLocation(previous)
		SetCodexProxyLocationResolver(nil)
	})
}

const webSearchLocationBody = `{"model":"gpt-5.5","input":"hi","tools":[{"type":"function","name":"f"},{"type":"web_search","user_location":{"type":"approximate","country":"CN","city":"Shanghai"}},{"type":"web_search_preview"}]}`

func TestApplyCodexWebSearchLocationUsesStoredProxyLocation(t *testing.T) {
	withCodexWebSearchLocation(t, true, map[string]database.ProxyLocation{
		"http://us.example:8080": {Country: "US", Region: "California", City: "Los Angeles", Timezone: "America/Los_Angeles"},
	})
	account := &auth.Account{DBID: 7, RefreshToken: "rt"}
	got := ApplyCodexWebSearchLocation([]byte(webSearchLocationBody), account, " http://us.example:8080 ")
	for _, index := range []string{"1", "2"} {
		location := gjson.GetBytes(got, "tools."+index+".user_location")
		if location.Get("type").String() != "approximate" || location.Get("country").String() != "US" ||
			location.Get("region").String() != "California" || location.Get("city").String() != "Los Angeles" ||
			location.Get("timezone").String() != "America/Los_Angeles" {
			t.Fatalf("tools.%s.user_location = %s", index, location.Raw)
		}
	}
	if gjson.GetBytes(got, "tools.0.user_location").Exists() {
		t.Fatal("non-search tools must not get a location")
	}
	if gjson.GetBytes(got, "input").String() != "hi" || len(gjson.GetBytes(got, "tools").Array()) != 3 {
		t.Fatal("input or tool list changed")
	}

	// The account's bound timezone wins over the proxy egress timezone.
	account.Timezone = "America/New_York"
	got = ApplyCodexWebSearchLocation([]byte(webSearchLocationBody), account, "http://us.example:8080")
	if tz := gjson.GetBytes(got, "tools.1.user_location.timezone").String(); tz != "America/New_York" {
		t.Fatalf("timezone = %q, want account timezone", tz)
	}
}

func TestApplyCodexWebSearchLocationKeepsClientValue(t *testing.T) {
	locations := map[string]database.ProxyLocation{
		"http://us.example:8080": {Country: "US"},
		"http://tz-only.example": {Timezone: "Asia/Tokyo"},
	}
	account := &auth.Account{DBID: 7, RefreshToken: "rt", Timezone: "Asia/Tokyo"}
	for name, tc := range map[string]struct {
		enabled  bool
		proxyURL string
	}{
		"disabled":      {false, "http://us.example:8080"},
		"direct":        {true, ""},
		"unknown proxy": {true, "http://unknown.example:1"},
		"timezone only": {true, "http://tz-only.example"},
	} {
		t.Run(name, func(t *testing.T) {
			withCodexWebSearchLocation(t, tc.enabled, locations)
			if got := ApplyCodexWebSearchLocation([]byte(webSearchLocationBody), account, tc.proxyURL); string(got) != webSearchLocationBody {
				t.Fatalf("body changed: %s", got)
			}
		})
	}
	withCodexWebSearchLocation(t, true, locations)
	noTools := `{"model":"gpt-5.5","input":"hi"}`
	if got := ApplyCodexWebSearchLocation([]byte(noTools), account, "http://us.example:8080"); string(got) != noTools {
		t.Fatalf("body without tools changed: %s", got)
	}
}

func TestCodexOutboundProxyURLPrefersOverride(t *testing.T) {
	account := &auth.Account{DBID: 7, ProxyURL: "http://account.example"}
	if got := codexOutboundProxyURL(account, " http://pool.example "); got != "http://pool.example" {
		t.Fatalf("override = %q", got)
	}
	if got := codexOutboundProxyURL(account, ""); got != "http://account.example" {
		t.Fatalf("account proxy = %q", got)
	}
}
