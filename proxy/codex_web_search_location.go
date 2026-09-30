package proxy

import (
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Web search location: when enabled, the approximate location of existing
// web_search tools follows the account's actual egress proxy instead of the
// downstream client. Locations are the persisted proxy test results kept in
// memory by auth.Store; the request path never queries a geo service or the
// database. Off by default.
var (
	codexWebSearchProxyLocationEnabled atomic.Bool
	codexProxyLocationResolver         atomic.Pointer[func(string) database.ProxyLocation]
)

// SetCodexWebSearchProxyLocation switches the rewrite on or off at runtime.
func SetCodexWebSearchProxyLocation(enabled bool) {
	codexWebSearchProxyLocationEnabled.Store(enabled)
}

// CodexWebSearchProxyLocationEnabled reports the runtime switch.
func CodexWebSearchProxyLocationEnabled() bool {
	return codexWebSearchProxyLocationEnabled.Load()
}

// SetCodexProxyLocationResolver installs the in-memory proxy location lookup.
func SetCodexProxyLocationResolver(resolve func(string) database.ProxyLocation) {
	if resolve == nil {
		codexProxyLocationResolver.Store(nil)
		return
	}
	codexProxyLocationResolver.Store(&resolve)
}

// codexOutboundProxyURL mirrors the executors' proxy priority: the pool/override
// proxy first, then the account's own proxy.
func codexOutboundProxyURL(account *auth.Account, proxyOverride string) string {
	if proxyOverride = strings.TrimSpace(proxyOverride); proxyOverride != "" || account == nil {
		return proxyOverride
	}
	account.Mu().RLock()
	defer account.Mu().RUnlock()
	return strings.TrimSpace(account.ProxyURL)
}

// ApplyCodexWebSearchLocation replaces tools[].user_location of top-level
// web_search / web_search_* tools with the outbound proxy's stored location.
// It never adds a tool or touches input. Direct connections, proxies outside
// the proxy table, Resin egress (exit unknown) and proxies without a stored
// location keep the client's value. The timezone follows the account's bound
// timezone first, so it stays consistent with environment_context.
func ApplyCodexWebSearchLocation(body []byte, account *auth.Account, proxyURL string) []byte {
	if !codexWebSearchProxyLocationEnabled.Load() || len(body) == 0 || account == nil || resinCarriesEgress(account) {
		return body
	}
	proxyURL = strings.TrimSpace(proxyURL)
	resolve := codexProxyLocationResolver.Load()
	if proxyURL == "" || resolve == nil {
		return body
	}
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body
	}
	location := (*resolve)(proxyURL)
	if timezone := account.EffectiveCodexTimezone(); timezone != "" {
		location.Timezone = timezone
	}
	if location.Country == "" && location.Region == "" && location.City == "" {
		// A timezone alone is not a location; keep the client's value.
		return body
	}
	value := map[string]string{"type": "approximate"}
	for key, field := range map[string]string{"country": location.Country, "region": location.Region, "city": location.City, "timezone": location.Timezone} {
		if field != "" {
			value[key] = field
		}
	}
	original := body
	for index, tool := range tools.Array() {
		kind := tool.Get("type").String()
		if kind != "web_search" && !strings.HasPrefix(kind, "web_search_") {
			continue
		}
		var err error
		body, err = sjson.SetBytes(body, "tools."+strconv.Itoa(index)+".user_location", value)
		if err != nil {
			return original
		}
	}
	return body
}
