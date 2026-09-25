package proxy

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

type upstreamTraceContextKey struct{}
type upstreamTraceAttempt struct {
	turnState  *usageTurnStateObservation
	idempotent bool
	accountID  int64
	requestID  string
	proxy      auth.ProxyAuditLabel
	transport  UpstreamTransportDiagnostic
}

type upstreamTraceSnapshot struct {
	IdentityClaim     *codexIdentityClaimDiagnostic
	RequestID         string
	accountID         int64
	UpstreamRequestID string
	Proxy             auth.ProxyAuditLabel
	Transport         *UpstreamTransportDiagnostic
}

func snapshotUpstreamTrace(ctx context.Context) upstreamTraceSnapshot {
	a := upstreamTraceFromContext(ctx)
	if a == nil {
		return upstreamTraceSnapshot{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	result := upstreamTraceSnapshot{RequestID: a.requestID, IdentityClaim: a.identityClaim}
	if a.current != nil {
		result.accountID = a.current.accountID
		result.UpstreamRequestID = a.current.requestID
		result.Proxy = a.current.proxy
		transport := a.current.transport
		transport.IdentityClaim = a.identityClaim
		result.Transport = &transport
	}
	return result
}

func (s upstreamTraceSnapshot) apply(input *database.UsageLogInput) {
	input.RequestID = s.RequestID
	if s.accountID == input.AccountID {
		input.UpstreamRequestID = s.UpstreamRequestID
		input.UpstreamProxyID = s.Proxy.ID
		input.UpstreamProxyName = s.Proxy.Name
		applyBPSUsageTransport(input, s.Transport)
		input.UpstreamDiagnostics = transportDiagnosticJSON(s.Transport)
		if s.Transport != nil {
			input.UpstreamResponseModel = s.Transport.ResponseModel
		}
	}
}

type upstreamTraceAudit struct {
	identityClaim *codexIdentityClaimDiagnostic
	mu            sync.Mutex
	requestID     string
	store         *auth.Store
	current       *upstreamTraceAttempt
}

func upstreamTraceFromContext(ctx context.Context) *upstreamTraceAudit {
	if ctx == nil {
		return nil
	}
	a, _ := ctx.Value(upstreamTraceContextKey{}).(*upstreamTraceAudit)
	return a
}

func attachUpstreamTrace(c *gin.Context, store *auth.Store) {
	if c == nil || c.Request == nil {
		return
	}
	a := &upstreamTraceAudit{requestID: NewUpstreamSessionUUID(), store: store}
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), upstreamTraceContextKey{}, a))
	c.Header("X-Codex2API-Request-ID", a.requestID)
}

func resetUpstreamRequestTrace(c *gin.Context) {
	if c == nil || c.Request == nil {
		return
	}
	if a := upstreamTraceFromContext(c.Request.Context()); a != nil {
		a.mu.Lock()
		a.requestID = NewUpstreamSessionUUID()
		a.current = nil
		a.identityClaim = nil
		a.mu.Unlock()
	}
}

func resetUpstreamAttemptTrace(ctx context.Context) {
	if a := upstreamTraceFromContext(ctx); a != nil {
		a.mu.Lock()
		a.current = nil
		a.identityClaim = nil
		a.mu.Unlock()
	}
}

func beginUpstreamTrace(ctx context.Context, account *auth.Account, proxyURL string, ws bool) func(*http.Response) {
	a := upstreamTraceFromContext(ctx)
	if a == nil || account == nil {
		return func(*http.Response) {}
	}
	label := a.store.ProxyAuditForURL(proxyURL)
	if ws && proxyURL == "" {
		label = auth.ProxyAuditLabel{Name: "unknown"}
	}
	if IsResinEnabled() && !account.IsRelayStyle() {
		label = auth.ProxyAuditLabel{Name: "resin"}
	}
	label.Name = security.MaskSensitiveData(label.Name)
	transport := "http"
	if ws {
		transport = "websocket"
	}
	egress := "direct"
	if proxyURL != "" {
		egress = "proxy"
	}
	if label.Name == "resin" {
		egress = "resin"
	}
	attempt := &upstreamTraceAttempt{accountID: account.ID(), proxy: label, transport: UpstreamTransportDiagnostic{
		AccountID: account.ID(), Transport: transport, EgressKind: egress, ProxyID: label.ID, ProxyName: label.Name,
		ProxyEndpoint: safeProxyEndpoint(proxyURL), PublicEgressIPStatus: "not_observed", SendPhase: "before_payload",
	}}
	attempt.transport.BPS, _ = ctx.Value(codexBPSDiagnosticKey{}).(*CodexBPSDiagnostic)
	attempt.turnState, _ = ctx.Value(usageTurnStateKey{}).(*usageTurnStateObservation)
	if mapping, ok := ctx.Value(codexAccountIdentityDiagnosticKey{}).(*codexAccountIdentityDiagnostic); ok {
		attempt.transport.OutboundIdentity = &outboundIdentityDiagnostic{FormatVersion: 2, AccountMapping: mapping}
	}
	if account.IsOpenAIResponsesAPI() {
		if attempt.transport.OutboundIdentity == nil {
			attempt.transport.OutboundIdentity = &outboundIdentityDiagnostic{FormatVersion: 2}
		}
		attempt.transport.OutboundIdentity.SessionHeaderPolicy = "optional"
	}
	if attempt.transport.BPS != nil {
		if attempt.transport.OutboundIdentity == nil {
			attempt.transport.OutboundIdentity = &outboundIdentityDiagnostic{FormatVersion: 2}
		}
		attempt.transport.OutboundIdentity.SessionHeaderPolicy = "bps"
	}
	a.mu.Lock()
	a.current = attempt
	a.mu.Unlock()
	if projects := projectIdentityFrom(ctx); projects != nil {
		projects.mu.Lock()
		projects.publish(ctx)
		projects.mu.Unlock()
	}
	if urls := codexURLState(ctx, account); urls != nil {
		urls.mu.Lock()
		urls.publish(ctx)
		urls.mu.Unlock()
	}
	header := account.GetUpstreamRequestIDHeader()
	observer := &TransportObserver{audit: a, attempt: attempt}
	return func(resp *http.Response) {
		if resp == nil || ws {
			return
		} // A WS handshake ID is not a per-turn ID.
		observer.ResponseHeaders(resp.StatusCode, resp.Header, false)
		id := ""
		if header != "" && auth.ValidateUpstreamRequestIDHeader(header) == nil {
			id = resp.Header.Get(header)
		} else if header == "" {
			id = upstreamHeaderRequestID(resp.Header)
		}
		id = security.SafeTruncate(strings.TrimSpace(id), 128)
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.current == attempt {
			attempt.requestID = id
			attempt.transport.UpstreamRequestID = safeDiagnosticToken(id)
		}
	}
}

func doTracedUpstreamRequest(client *http.Client, req *http.Request, account *auth.Account, proxyURL string, jsonBodies ...[]byte) (*http.Response, error) {
	req = req.WithContext(ensureTransportTrace(req.Context()))
	record := beginUpstreamTrace(req.Context(), account, proxyURL, false)
	observer := UpstreamTransportObserver(req.Context())
	observer.OutboundHTTPIdentity(req.Header)
	if len(jsonBodies) > 0 {
		observer.ResponsesInput(jsonBodies[0], req.Header, req.URL.Path)
	}
	observer.update(func(*UpstreamTransportDiagnostic) {
		observer.attempt.idempotent = req.Method == http.MethodGet || req.Method == http.MethodHead || req.Method == http.MethodOptions
	})
	observer.Endpoint(req.URL.String())
	observer.update(func(d *UpstreamTransportDiagnostic) {
		if d.BPS != nil {
			d.UpstreamEndpoint = CodexBPSBaseURL + "/responses"
			if d.BPS.Compact {
				d.UpstreamEndpoint += "/compact"
			}
		}
	})
	timing := bpsTimingFromContext(req.Context())
	timing.startInference(time.Now())
	traced, network := traceBPSHTTP(traceHTTPTransport(req, observer))
	resp, err := client.Do(traced)
	phases := network.finish(err)
	timing.update(func(v *bpsTimingValues) { v.LastInferenceHTTP = phases })
	if resp != nil {
		timing.receivedHeaders(time.Now())
	}
	record(resp)
	if observer != nil && resp != nil && resp.Body != nil {
		resp.Body = &tracedResponseBody{ReadCloser: resp.Body, observer: observer, captureError: resp.StatusCode >= 400,
			requireTerminal: resp.StatusCode < 300 && strings.HasSuffix(req.URL.Path, "/responses") && strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")}
	}
	if err != nil {
		observer.Failure("transport", "http_roundtrip", 0)
		err = observer.TransportError(err)
	}
	return resp, err
}

func populateUpstreamTrace(c *gin.Context, input *database.UsageLogInput) {
	if c == nil || c.Request == nil || input == nil {
		return
	}
	if input.RequestID != "" {
		return
	} // Hidden continuation rounds carry their own snapshot.
	a := upstreamTraceFromContext(c.Request.Context())
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	input.RequestID = a.requestID
	if current := a.current; current != nil && current.accountID == input.AccountID {
		input.UpstreamRequestID = current.requestID
		input.UpstreamProxyID = current.proxy.ID
		input.UpstreamProxyName = current.proxy.Name
		applyBPSUsageTransport(input, &current.transport)
		transport := current.transport
		transport.IdentityClaim = a.identityClaim
		input.UpstreamDiagnostics = transportDiagnosticJSON(&transport)
		input.UpstreamResponseModel = current.transport.ResponseModel
	}
}
