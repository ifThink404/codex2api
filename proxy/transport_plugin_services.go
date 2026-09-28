package proxy

import (
	"context"
	"net/http"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy/plugins"
)

// coreServices is the plugins.Services implementation: the same client pool,
// Resin routing, upstream trace, UA audit and model quota as ExecuteRequest.
type coreServices struct{}

func init() { plugins.Default().SetServices(coreServices{}) }

func (coreServices) HTTPClient(account *auth.Account, proxyURL string) (*http.Client, func(string) string) {
	if IsResinEnabled() {
		return getResinHTTPClient(account), BuildReverseProxyURL
	}
	return getPooledClient(account, proxyURL), func(url string) string { return url }
}

func (coreServices) PrepareRequest(req *http.Request, account *auth.Account) {
	if IsResinEnabled() {
		req.Header.Set("X-Resin-Account", ResinAccountID(account))
	}
}

func (coreServices) Do(client *http.Client, req *http.Request, account *auth.Account, proxyURL string) (*http.Response, error) {
	return doTracedUpstreamRequest(client, req, account, proxyURL)
}

func (coreServices) RecordUserAgent(ctx context.Context, userAgent string) {
	RecordUpstreamUserAgent(ctx, userAgent)
}

func (coreServices) ConsumeModelQuota(ctx context.Context, model string) error {
	return ConsumeAPIKeyModelRequestQuota(ctx, model)
}

func (coreServices) RecycleClient(account *auth.Account, proxyURL string, err error) {
	if shouldRecyclePooledClient(err) {
		recyclePooledClient(account, proxyURL)
	}
}

// pluginServices returns env.Services, falling back to core's for callers
// that build a ReqEnv without a Route (tests).
func pluginServices(env *plugins.ReqEnv) plugins.Services {
	if env != nil && env.Services != nil {
		return env.Services
	}
	return coreServices{}
}
