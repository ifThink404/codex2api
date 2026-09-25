package proxy

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type rawRelayDiagnostic struct {
	Enabled       bool   `json:"enabled"`
	RequestBytes  int    `json:"request_bytes"`
	ResponseBytes int64  `json:"response_bytes"`
	UsageSource   string `json:"usage_source"`
	ResponseError string `json:"response_error,omitempty"`
}

// The HTTP handlers call this only after tryRawRelay declined the original
// model. A later alias/effort mapping must not route a transformed request to
// an account whose administrator opted into raw transport.
func excludeRawRelayAccountsFilter(filter auth.AccountFilter, trace *auth.SelectionTrace) auth.AccountFilter {
	return func(account *auth.Account) bool {
		if account == nil {
			return false
		}
		if account.OpenAIRawPassthroughEnabled() {
			if trace != nil {
				trace.RejectAccount(account.ID(), "model_or_provider_mismatch")
			}
			return false
		}
		return filter == nil || filter(account)
	}
}

// An explicitly configured raw API route is selected before Codex validation or
// translation. Only models and caller/account permissions determine eligibility.
// Matching raw routes take precedence; failures never fall back to a transformer.
func (h *Handler) tryRawRelay(c *gin.Context) bool {
	if h.store == nil || c.Request.Method != http.MethodPost {
		return false
	}
	var rawAccounts []*auth.Account
	keyID := requestAPIKeyID(c)
	for _, account := range h.store.Accounts() {
		if account.OpenAIRawPassthroughEnabled() && account.AllowsAPIKey(keyID) && h.store.APIKeyAllowsAccount(keyID, account) {
			rawAccounts = append(rawAccounts, account)
		}
	}
	if len(rawAccounts) == 0 {
		return false
	}
	body, ok := rawRequestBodyFromContext(c)
	if !ok {
		var err error
		body, err = io.ReadAll(io.LimitReader(c.Request.Body, int64(security.MaxRequestBodySize)+1))
		if err != nil {
			api.SendError(c, requestBodyReadAPIError(err))
			return true
		}
		setRawRequestBody(c, body)
	}
	if len(body) > security.MaxRequestBodySize {
		api.SendErrorWithStatus(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Request body is too large", api.ErrorTypeInvalidRequest), http.StatusRequestEntityTooLarge)
		return true
	}
	routingBody := body
	if strings.EqualFold(strings.TrimSpace(c.GetHeader("Content-Encoding")), "gzip") {
		reader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			api.SendError(c, requestBodyReadAPIError(err))
			return true
		}
		routingBody, err = io.ReadAll(io.LimitReader(reader, int64(security.MaxRequestBodySize)+1))
		reader.Close()
		if err != nil || len(routingBody) > security.MaxRequestBodySize {
			api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Invalid or oversized compressed request", api.ErrorTypeInvalidRequest))
			return true
		}
	}
	model := strings.TrimSpace(gjson.GetBytes(routingBody, "model").String())
	eligible := map[int64]bool{}
	for _, account := range rawAccounts {
		if account.SupportsOpenAIResponsesModel(model) {
			eligible[account.ID()] = true
		}
	}
	if len(eligible) == 0 {
		return false
	}
	// A unique routing model is necessary to enforce the caller's model policy.
	modelFields := 0
	gjson.ParseBytes(routingBody).ForEach(func(key, value gjson.Result) bool {
		if key.String() == "model" {
			modelFields++
		}
		return true
	})
	if !gjson.ValidBytes(routingBody) || !gjson.ParseBytes(routingBody).IsObject() || modelFields != 1 || gjson.GetBytes(routingBody, "model").Type != gjson.String {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Exactly one model field is required for routing", api.ErrorTypeInvalidRequest))
		return true
	}
	started := time.Now()
	diagnostic := &rawRelayDiagnostic{Enabled: true, RequestBytes: len(body), UsageSource: "not_observed"}
	usageRequestDiagnosticState(c).RawPassthrough = diagnostic
	setIngressRequestBodyIfAbsent(c, body)
	cacheTrustedRequestedModel(c, model)
	c.Set("x-model", model)
	h.primeNewAPIPolicyContext(c, body)
	status, signed := h.cachedNewAPIPolicyAuditState(c)
	bindTransportOwner(c, signed, (status == "verified" || status == "signed_response") && signed.MetaVerified)
	if h.enforceAPIKeyLimitsAndReply(c, model) {
		return true
	}
	release, admitted := h.acquireAPIKeyConcurrency(c)
	if !admitted {
		return true
	}
	if release != nil {
		defer release()
	}
	defer h.ReleaseAPIKeyScopeConcurrency(c)
	filter := auth.AccountFilter(func(account *auth.Account) bool {
		return eligible[account.ID()] && account.OpenAIRawPassthroughEnabled()
	})
	filter = h.applyUpstreamChannelFilter(c, model, filter)
	filter = h.withRequestModelCooldownFilter(c, model, filter)
	filter = h.applyScopeBudgetFilter(c, filter)
	beginDispatchSelection(c)
	account := h.store.NextExcludingWithDispatch(keyID, nil, filter, auth.DispatchPolicyStandard.WithModel(model), selectionTraceForRequest(c))
	if account == nil {
		if msg := scopeBudgetExhaustedMessage(c); msg != "" {
			SendAPIKeyLimitError(c, http.StatusTooManyRequests, msg)
		} else {
			api.SendErrorWithStatus(c, api.NewAPIError(api.ErrCodeServiceUnavailable, "No eligible raw API account is available", api.ErrorTypeServer), http.StatusServiceUnavailable)
		}
		return true
	}
	defer h.store.Release(account)
	h.AcquireAPIKeyScopeConcurrency(c, account)
	if err := ConsumeAPIKeyModelRequestQuota(c.Request.Context(), model); err != nil {
		ErrorToGinResponse(c, err)
		return true
	}
	proxyURL, usable := h.store.ResolveUsableProxyForAccount(account)
	if !usable {
		api.SendErrorWithStatus(c, api.NewAPIError(api.ErrCodeServiceUnavailable, "Raw API egress is unavailable", api.ErrorTypeServer), http.StatusServiceUnavailable)
		return true
	}
	baseURL, credential := account.OpenAIResponsesCredentials()
	endpoint := auth.OpenAIResponsesEndpoint(baseURL, c.Request.URL.EscapedPath())
	if c.Request.URL.RawQuery != "" {
		endpoint += "?" + c.Request.URL.RawQuery
	}
	client, err := rawRelayClient(account, proxyURL)
	if err != nil {
		api.SendErrorWithStatus(c, api.NewAPIError(api.ErrCodeServiceUnavailable, "Raw API transport configuration is invalid", api.ErrorTypeServer), http.StatusServiceUnavailable)
		return true
	}
	req, err := http.NewRequestWithContext(c.Request.Context(), c.Request.Method, endpoint, bytes.NewReader(body))
	if err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Invalid upstream endpoint", api.ErrorTypeInvalidRequest))
		return true
	}
	req.Header = rawRelayRequestHeaders(c.Request.Header, account, credential)
	recordTrace := beginUpstreamTrace(req.Context(), account, proxyURL, false)
	UpstreamTransportObserver(req.Context()).Endpoint(endpoint)
	resp, err := client.Do(req)
	recordTrace(resp)
	input := &database.UsageLogInput{AccountID: account.ID(), Endpoint: c.Request.URL.Path, InboundEndpoint: c.Request.URL.Path, UpstreamEndpoint: c.Request.URL.Path, Model: model, EffectiveModel: model, Stream: gjson.GetBytes(routingBody, "stream").Bool(), AttemptIndex: 1, ReasoningEffort: extractReasoningEffort(routingBody)}
	defer func() {
		input.DurationMs = int(time.Since(started).Milliseconds())
		if h.db != nil {
			h.logUsageForRequest(c, input)
		}
	}()
	if err != nil {
		input.StatusCode = http.StatusBadGateway
		input.ErrorMessage = "Raw API transport failed"
		UpstreamTransportObserver(req.Context()).Failure("upstream_transport", "request", 0)
		api.SendErrorWithStatus(c, api.NewAPIError(api.ErrCodeServiceUnavailable, input.ErrorMessage, api.ErrorTypeServer), http.StatusBadGateway)
		return true
	}
	defer resp.Body.Close()
	input.StatusCode = resp.StatusCode
	for name, values := range rawRelayResponseHeaders(resp.Header) {
		c.Writer.Header()[name] = append([]string(nil), values...)
	}
	for name := range resp.Trailer {
		c.Writer.Header().Add("Trailer", name)
	}
	c.Status(resp.StatusCode)
	c.Writer.WriteHeaderNow()
	observer := &rawRelayUsageObserver{stream: strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream"), encoding: strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))}
	buffer := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			if diagnostic.ResponseBytes == 0 {
				input.FirstTokenMs = int(time.Since(started).Milliseconds())
			}
			written, writeErr := c.Writer.Write(buffer[:n])
			diagnostic.ResponseBytes += int64(written)
			observer.Write(buffer[:written])
			if observer.stream {
				c.Writer.Flush()
			}
			if writeErr != nil {
				diagnostic.ResponseError = "downstream_write_failed"
				break
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				diagnostic.ResponseError = "upstream_read_failed"
			}
			break
		}
	}
	observer.finish()
	for name, values := range resp.Trailer {
		c.Writer.Header()[http.TrailerPrefix+name] = append([]string(nil), values...)
	}
	if observer.usage != nil {
		u := observer.usage
		input.InputTokens, input.OutputTokens, input.CachedTokens = u.InputTokens, u.OutputTokens, u.CachedTokens
		input.PromptTokens, input.CompletionTokens, input.TotalTokens = u.PromptTokens, u.CompletionTokens, u.TotalTokens
		input.ReasoningTokens = u.ReasoningTokens
		input.CacheWrite5mTokens, input.CacheWrite1hTokens = u.CacheWrite5mTokens, u.CacheWrite1hTokens
		diagnostic.UsageSource = "upstream"
	} else if observer.disabled {
		diagnostic.UsageSource = "observation_limit_or_encoding"
	}
	input.UpstreamResponseModel = observer.model
	tiers := resolveUsageServiceTiers(observer.tier, gjson.GetBytes(routingBody, "service_tier").String())
	input.ServiceTier, input.RequestedServiceTier, input.ActualServiceTier, input.BillingServiceTier = tiers.ServiceTier, tiers.RequestedServiceTier, tiers.ActualServiceTier, tiers.BillingServiceTier
	if resp.StatusCode >= 400 {
		input.ErrorMessage = observer.errorMessage
		if input.ErrorMessage == "" {
			input.ErrorMessage = fmt.Sprintf("Upstream HTTP %d", resp.StatusCode)
		}
	}
	if diagnostic.ResponseError != "" {
		input.ErrorMessage = diagnostic.ResponseError
	}
	return true
}

func rawRelayClient(account *auth.Account, proxyURL string) (*http.Client, error) {
	key := "raw-api:" + clientPoolKey(account, proxyURL, codexTransportModeStandard)
	if value, ok := clientPool.Load(key); ok {
		entry := value.(*poolEntry)
		entry.touch()
		return entry.client, nil
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	transport.Proxy = nil
	transport.ResponseHeaderTimeout = 5 * time.Minute
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = dialer.DialContext
	if err := auth.ConfigureTransportProxy(transport, proxyURL, dialer); err != nil {
		return nil, fmt.Errorf("invalid proxy configuration")
	}
	entry := &poolEntry{createdAt: time.Now().UnixNano(), client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	entry.touch()
	if value, loaded := clientPool.LoadOrStore(key, entry); loaded {
		transport.CloseIdleConnections()
		return value.(*poolEntry).client, nil
	}
	return entry.client, nil
}

func rawRelayResponseHeaders(source http.Header) http.Header {
	headers := source.Clone()
	for _, value := range source.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			headers.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		headers.Del(name)
	}
	return headers
}

func rawRelayRequestHeaders(source http.Header, account *auth.Account, credential string) http.Header {
	headers := rawRelayResponseHeaders(source)
	for name := range headers {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-newapi-") || strings.HasPrefix(lower, "x-codex2api-") || strings.HasPrefix(lower, "x-forwarded-") {
			headers.Del(name)
		}
	}
	for _, name := range []string{"Authorization", "X-Api-Key", "Api-Key", "Cookie", "Forwarded", "Content-Length", "Host"} {
		headers.Del(name)
	}
	account.Mu().RLock()
	for name, value := range account.CustomHeaders {
		headers.Set(name, value)
	}
	account.Mu().RUnlock()
	headers = rawRelayResponseHeaders(headers)
	headers.Set("Authorization", "Bearer "+credential)
	if _, ok := headers["User-Agent"]; !ok {
		headers["User-Agent"] = []string{""}
	}
	return headers
}

// Observe a bounded copy; never reconstruct, suppress or inject response bytes.
type rawRelayUsageObserver struct {
	stream, disabled, discardLine bool
	pending                       []byte
	usage                         *UsageInfo
	encoding                      string
	compressed                    []byte
	model, tier, errorMessage     string
}

func (o *rawRelayUsageObserver) Write(data []byte) {
	if o.disabled {
		return
	}
	if o.encoding != "" && o.encoding != "identity" {
		if o.encoding != "gzip" || len(o.compressed)+len(data) > 4<<20 {
			o.disabled = true
			o.compressed = nil
			return
		}
		o.compressed = append(o.compressed, data...)
		return
	}
	if !o.stream {
		if len(o.pending)+len(data) > 4<<20 {
			o.disabled = true
			o.pending = nil
			return
		}
		o.pending = append(o.pending, data...)
		return
	}
	for len(data) > 0 {
		at := bytes.IndexByte(data, '\n')
		chunk := data
		if at >= 0 {
			chunk = data[:at]
		}
		if !o.discardLine {
			if len(o.pending)+len(chunk) > 4<<20 {
				o.pending = nil
				o.discardLine = true
			} else {
				o.pending = append(o.pending, chunk...)
			}
		}
		if at < 0 {
			return
		}
		if !o.discardLine {
			line := bytes.TrimSpace(o.pending)
			if bytes.HasPrefix(line, []byte("data:")) {
				o.observe(bytes.TrimSpace(line[5:]))
			}
		}
		o.pending = nil
		o.discardLine = false
		data = data[at+1:]
	}
}

func (o *rawRelayUsageObserver) finish() {
	if o.disabled || o.discardLine {
		return
	}
	if o.encoding == "gzip" {
		reader, err := gzip.NewReader(bytes.NewReader(o.compressed))
		if err != nil {
			o.disabled = true
			return
		}
		decoded, err := io.ReadAll(io.LimitReader(reader, (4<<20)+1))
		reader.Close()
		o.compressed = nil
		if err != nil || len(decoded) > 4<<20 {
			o.disabled = true
			return
		}
		o.encoding = ""
		o.Write(decoded)
	}
	if !o.stream {
		o.observe(o.pending)
		return
	}
	line := bytes.TrimSpace(o.pending)
	if bytes.HasPrefix(line, []byte("data:")) {
		o.observe(bytes.TrimSpace(line[5:]))
	}
}

func (o *rawRelayUsageObserver) observe(body []byte) {
	root := gjson.ParseBytes(body)
	meta := root
	if response := root.Get("response"); response.IsObject() {
		meta = response
	} else if message := root.Get("message"); message.IsObject() {
		meta = message
	}
	if model := meta.Get("model"); model.Type == gjson.String {
		o.model = model.String()
	}
	if tier := meta.Get("service_tier"); tier.Type == gjson.String {
		o.tier = tier.String()
	}
	if message := meta.Get("error.message"); message.Type == gjson.String {
		o.errorMessage = message.String()
		if len(o.errorMessage) > 4096 {
			o.errorMessage = o.errorMessage[:4096]
		}
	}
	usage := gjson.GetBytes(body, "usage")
	if !usage.IsObject() {
		usage = gjson.GetBytes(body, "response.usage")
	}
	if !usage.IsObject() {
		usage = gjson.GetBytes(body, "message.usage")
	}
	if !usage.IsObject() {
		return
	}
	if usage.Get("prompt_tokens").Exists() {
		o.usage = newUsageInfo(int(usage.Get("prompt_tokens").Int()), int(usage.Get("completion_tokens").Int()), int(usage.Get("completion_tokens_details.reasoning_tokens").Int()), int(usage.Get("prompt_tokens_details.cached_tokens").Int()))
	} else if gjson.GetBytes(body, "type").String() == "message_delta" && o.usage != nil {
		// Anthropic deltas report cumulative output without repeating input usage.
		if value := usage.Get("output_tokens"); value.Exists() {
			o.usage.OutputTokens = int(value.Int())
			o.usage.CompletionTokens = o.usage.OutputTokens
			o.usage.TotalTokens = o.usage.InputTokens + o.usage.OutputTokens
		}
	} else {
		o.usage = extractUsageFromResult(usage)
		if cached := usage.Get("cache_read_input_tokens"); cached.Exists() || usage.Get("cache_creation_input_tokens").Exists() {
			o.usage.CachedTokens = int(cached.Int())
			o.usage.CacheWriteTokens = int(usage.Get("cache_creation_input_tokens").Int())
			o.usage.CacheWrite1hTokens = int(usage.Get("cache_creation.ephemeral_1h_input_tokens").Int())
			o.usage.CacheWrite5mTokens = max(0, o.usage.CacheWriteTokens-o.usage.CacheWrite1hTokens)
			o.usage.InputTokens += o.usage.CachedTokens + o.usage.CacheWriteTokens
			o.usage.PromptTokens = o.usage.InputTokens
			o.usage.TotalTokens = o.usage.InputTokens + o.usage.OutputTokens
		}
	}
}
