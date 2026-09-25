package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"time"

	"github.com/codex2api/security"
	"github.com/tidwall/gjson"
)

type UpstreamTransportDiagnostic struct {
	IdentityClaim          *codexIdentityClaimDiagnostic      `json:"identity_claim,omitempty"`
	ClientTurnState        *usageTurnStateValue               `json:"client_turn_state,omitempty"`
	BPS                    *CodexBPSDiagnostic                `json:"bps_compat,omitempty"`
	RequestTurnState       *usageTurnStateValue               `json:"request_turn_state,omitempty"`
	ResponseTurnState      *usageTurnStateValue               `json:"response_turn_state,omitempty"`
	AccessPrograms         *accessProgramsValueDiagnostic     `json:"access_programs,omitempty"`
	ResponseModel          string                             `json:"response_model,omitempty"`
	ReasoningEffort        string                             `json:"reasoning_effort,omitempty"`
	ResponseModelConflict  bool                               `json:"response_model_conflict,omitempty"`
	StreamDelivery         *ResponsesStreamDeliveryDiagnostic `json:"stream_delivery,omitempty"`
	OutboundIdentity       *outboundIdentityDiagnostic        `json:"outbound_identity,omitempty"`
	ResponsesInput         *responsesInputDiagnostic          `json:"responses_input,omitempty"`
	ToolProtocol           *toolProtocolDiagnostic            `json:"tool_protocol,omitempty"`
	AccountID              int64                              `json:"account_id,omitempty"`
	Transport              string                             `json:"transport"`
	UpstreamEndpoint       string                             `json:"upstream_endpoint,omitempty"`
	EgressKind             string                             `json:"egress_kind"`
	ProxyID                int64                              `json:"proxy_id,omitempty"`
	ProxyName              string                             `json:"proxy_name,omitempty"`
	ProxyEndpoint          string                             `json:"proxy_endpoint,omitempty"`
	PublicEgressIPStatus   string                             `json:"public_egress_ip_status"`
	TCPPeer                string                             `json:"tcp_peer,omitempty"`
	ConnectionID           string                             `json:"connection_id,omitempty"`
	ConnectionState        string                             `json:"connection_state,omitempty"`
	ConnectionReused       *bool                              `json:"connection_reused,omitempty"`
	ConnectionAgeMillis    int64                              `json:"connection_age_millis,omitempty"`
	ConnectionProfile      string                             `json:"connection_profile,omitempty"`
	RequestedProfile       string                             `json:"requested_connection_profile,omitempty"`
	ProfileMatch           *bool                              `json:"profile_match,omitempty"`
	PoolKeyHash            string                             `json:"pool_key_hash,omitempty"`
	ContinuationMode       string                             `json:"continuation_mode,omitempty"`
	ContinuationResult     string                             `json:"continuation_result,omitempty"`
	SendPhase              string                             `json:"send_phase"`
	ErrorSource            string                             `json:"error_source,omitempty"`
	ErrorStage             string                             `json:"error_stage,omitempty"`
	ErrorCode              string                             `json:"error_code,omitempty"`
	ErrorType              string                             `json:"error_type,omitempty"`
	ErrorDetail            *upstreamErrorDiagnostic           `json:"error_detail,omitempty"`
	ErrorResponse          *upstreamErrorResponseDiagnostic   `json:"error_response,omitempty"`
	RateLimitHeaders       map[string]string                  `json:"rate_limit_headers,omitempty"`
	EventStatus            int                                `json:"event_status,omitempty"`
	HTTPStatus             int                                `json:"http_status,omitempty"`
	HandshakeStatus        int                                `json:"handshake_status,omitempty"`
	HandshakeRequestID     string                             `json:"handshake_request_id,omitempty"`
	UpstreamRequestID      string                             `json:"upstream_request_id,omitempty"`
	IdentityErrorCode      string                             `json:"identity_error_code,omitempty"`
	AuthorizationError     string                             `json:"authorization_error,omitempty"`
	CFRay                  string                             `json:"cf_ray,omitempty"`
	CloseCode              int                                `json:"close_code,omitempty"`
	ReplayBlocked          bool                               `json:"replay_blocked,omitempty"`
	MessageTooBigSource    string                             `json:"message_too_big_source,omitempty"`
	ReplayDecision         string                             `json:"replay_decision,omitempty"`
	HTTPSizeRouteLearned   bool                               `json:"http_size_route_learned,omitempty"`
	TransportOwnerHash     string                             `json:"transport_owner_hash,omitempty"`
	DownstreamConnectionID string                             `json:"downstream_connection_id,omitempty"`
	FailureCategory        string                             `json:"failure_category,omitempty"`
	FailureEvidence        string                             `json:"failure_evidence,omitempty"`
	LostConnection         *WebsocketConnectionLifecycle      `json:"lost_connection,omitempty"`
}

type TransportObserver struct {
	audit   *upstreamTraceAudit
	attempt *upstreamTraceAttempt
}

func UpstreamTransportObserver(ctx context.Context) *TransportObserver {
	audit := upstreamTraceFromContext(ctx)
	if audit == nil {
		return nil
	}
	audit.mu.Lock()
	defer audit.mu.Unlock()
	if audit.current == nil {
		return nil
	}
	return &TransportObserver{audit: audit, attempt: audit.current}
}

func (observer *TransportObserver) update(change func(*UpstreamTransportDiagnostic)) {
	if observer == nil {
		return
	}
	observer.audit.mu.Lock()
	defer observer.audit.mu.Unlock()
	if observer.audit.current == observer.attempt {
		change(&observer.attempt.transport)
		classifyTransportDiagnostic(&observer.attempt.transport)
	}
}

func safeDiagnosticToken(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) > 128 || strings.HasPrefix(value, "eyJ") || strings.HasPrefix(value, "sk-") {
		return "hash:" + hashRiskIdentity(value)
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("_-.:", character)) {
			return "hash:" + hashRiskIdentity(value)
		}
	}
	return value
}

func safeProxyEndpoint(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" {
		return ""
	}
	return strings.ToLower(parsed.Scheme) + "://" + parsed.Host
}

func upstreamHeaderRequestID(headers http.Header) string {
	for _, name := range []string{"X-Request-Id", "X-Oai-Request-Id", "X-Openai-Request-Id", "Openai-Request-Id", "Request-Id", "X-Goog-Request-Id"} {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			return safeDiagnosticToken(value)
		}
	}
	return ""
}

func (observer *TransportObserver) ResponseHeaders(status int, headers http.Header, handshake bool) {
	observer.update(func(diagnostic *UpstreamTransportDiagnostic) {
		if handshake {
			diagnostic.HandshakeStatus = status
			diagnostic.HandshakeRequestID = upstreamHeaderRequestID(headers)
			if status == http.StatusSwitchingProtocols && diagnostic.ErrorStage == "ws_handshake" {
				diagnostic.ErrorSource, diagnostic.ErrorStage = "", ""
			}
		} else {
			diagnostic.SendPhase = "after_payload"
			diagnostic.HTTPStatus = status
			diagnostic.UpstreamRequestID = upstreamHeaderRequestID(headers)
			if observer.attempt.requestID == "" {
				observer.attempt.requestID = diagnostic.UpstreamRequestID
			}
		}
		diagnostic.CFRay = safeDiagnosticToken(headers.Get("Cf-Ray"))
		diagnostic.RateLimitHeaders = upstreamRateLimitHeaders(headers)
		diagnostic.AuthorizationError = safeDiagnosticToken(headers.Get("X-OpenAI-Authorization-Error"))
		diagnostic.IdentityErrorCode = ""
		if encoded := headers.Get("X-Error-Json"); len(encoded) <= 8192 {
			if decoded, err := base64.StdEncoding.DecodeString(encoded); err == nil {
				diagnostic.IdentityErrorCode = safeDiagnosticToken(gjson.GetBytes(decoded, "error.code").String())
			}
		}
		if status >= 400 {
			diagnostic.ErrorSource, diagnostic.ErrorStage = "upstream_http", "http_response"
			if handshake {
				diagnostic.ErrorStage = "ws_handshake"
			}
		}
	})
}

func (observer *TransportObserver) Phase(phase string) {
	observer.update(func(diagnostic *UpstreamTransportDiagnostic) {
		if diagnostic.SendPhase != "after_payload" {
			diagnostic.SendPhase = phase
		}
	})
}

func (observer *TransportObserver) Failure(source, stage string, closeCode int) {
	observer.update(func(diagnostic *UpstreamTransportDiagnostic) {
		diagnostic.ErrorSource, diagnostic.ErrorStage, diagnostic.CloseCode = source, stage, closeCode
		if source == "transport" {
			diagnostic.ConnectionState = "failed"
		}
	})
}

func (observer *TransportObserver) FailureIfUnset(source, stage string) {
	observer.update(func(diagnostic *UpstreamTransportDiagnostic) {
		if diagnostic.ErrorSource == "" {
			diagnostic.ErrorSource, diagnostic.ErrorStage = source, stage
			if source == "transport" {
				diagnostic.ConnectionState = "failed"
			}
		}
	})
}

func (observer *TransportObserver) HandshakeProfile(profile string) {
	observer.update(func(diagnostic *UpstreamTransportDiagnostic) {
		diagnostic.RequestedProfile = profile
	})
}

func (observer *TransportObserver) Isolation(owner, downstreamID string) {
	observer.update(func(diagnostic *UpstreamTransportDiagnostic) {
		diagnostic.TransportOwnerHash, diagnostic.DownstreamConnectionID = owner, downstreamID
	})
}

func (observer *TransportObserver) Endpoint(endpoint string) {
	observer.update(func(diagnostic *UpstreamTransportDiagnostic) {
		diagnostic.UpstreamEndpoint = safeProxyEndpoint(endpoint)
	})
}

func (observer *TransportObserver) Continuation(mode, result string) {
	observer.update(func(diagnostic *UpstreamTransportDiagnostic) {
		diagnostic.ContinuationMode, diagnostic.ContinuationResult = mode, result
	})
}

func (observer *TransportObserver) Connection(id, poolKey, profile string, matches, reused bool, createdAt int64, peer, proxyURL string) {
	observer.update(func(diagnostic *UpstreamTransportDiagnostic) {
		diagnostic.ConnectionID, diagnostic.PoolKeyHash, diagnostic.ConnectionProfile = id, hashRiskIdentity(poolKey), profile
		diagnostic.ConnectionReused, diagnostic.ProfileMatch = &reused, &matches
		diagnostic.ConnectionState = "connected"
		if createdAt > 0 {
			diagnostic.ConnectionAgeMillis = max(time.Since(time.Unix(0, createdAt)).Milliseconds(), 0)
		}
		diagnostic.TCPPeer = peer
		if diagnostic.EgressKind != "resin" {
			diagnostic.EgressKind = "direct"
			if proxyURL != "" {
				diagnostic.EgressKind = "proxy"
			}
			diagnostic.ProxyEndpoint = safeProxyEndpoint(proxyURL)
			label := observer.audit.store.ProxyAuditForURL(proxyURL)
			label.Name = security.MaskSensitiveData(label.Name)
			observer.attempt.proxy = label
			diagnostic.ProxyID, diagnostic.ProxyName = label.ID, label.Name
		}
	})
}

func (observer *TransportObserver) Event(payload []byte) {
	observer.event("", payload)
}

func (observer *TransportObserver) event(name string, payload []byte) {
	observer.update(func(diagnostic *UpstreamTransportDiagnostic) {
		diagnostic.SendPhase = "after_payload"
		for _, path := range []string{"request_id", "response.request_id", "error.request_id"} {
			if value := gjson.GetBytes(payload, path).String(); value != "" {
				diagnostic.UpstreamRequestID = safeDiagnosticToken(value)
				observer.attempt.requestID = diagnostic.UpstreamRequestID
				break
			}
		}
		eventType := gjson.GetBytes(payload, "type").String()
		if eventType == "" {
			eventType = name
		}
		if diagnostic.BPS != nil && eventType != "" {
			content := isFirstTokenPayload(payload)
			if gjson.GetBytes(payload, "type").String() == "" {
				content = isFirstTokenEvent(eventType)
			}
			diagnostic.BPS.Timing.event(time.Now(), content)
		}
		observeUpstreamResponseModel(observer.attempt, payload, eventType)
		if eventType == "error" || eventType == "response.failed" {
			for _, path := range []string{"status_code", "status", "response.status_code", "error.status_code", "response.error.status_code"} {
				if status := int(gjson.GetBytes(payload, path).Int()); status >= 400 && status <= 599 {
					diagnostic.EventStatus = status
					break
				}
			}
			diagnostic.ErrorSource, diagnostic.ErrorStage = "upstream_ws", "ws_event"
			if diagnostic.Transport == "http" {
				diagnostic.ErrorSource, diagnostic.ErrorStage = "upstream_sse", "sse_event"
			}
			object := gjson.GetBytes(payload, "error")
			if !object.Exists() {
				object = gjson.GetBytes(payload, "response.error")
			}
			if !object.IsObject() {
				object = gjson.ParseBytes(payload)
			}
			diagnostic.ErrorCode = safeDiagnosticToken(object.Get("code").String())
			diagnostic.ErrorType = safeDiagnosticToken(object.Get("type").String())
		}
	})
}

type tracedResponseBody struct {
	io.ReadCloser
	observer        *TransportObserver
	errorBody       []byte
	captureError    bool
	requireTerminal bool
}

func (body *tracedResponseBody) Read(buffer []byte) (int, error) {
	count, err := body.ReadCloser.Read(buffer)
	if body.captureError && len(body.errorBody) < 8192 {
		body.errorBody = append(body.errorBody, buffer[:min(count, 8192-len(body.errorBody))]...)
		body.observer.HTTPErrorBody(body.errorBody)
	}
	if err != nil && err != io.EOF {
		body.observer.FailureIfUnset("transport", "http_body_read")
		err = body.observer.TransportError(err)
	}
	return count, err
}

func (body *tracedResponseBody) observeUpstreamEvent(event string, payload []byte) {
	body.observer.event(event, payload)
}

func (body *tracedResponseBody) finishObservedSSE(terminal bool) error {
	if body.requireTerminal && !terminal {
		body.observer.FailureIfUnset("transport", "sse_unexpected_eof")
		return body.observer.TransportError(io.ErrUnexpectedEOF)
	}
	return nil
}

type tracedRequestBody struct {
	io.ReadCloser
	observer *TransportObserver
}

func (body *tracedRequestBody) Read(buffer []byte) (int, error) {
	count, err := body.ReadCloser.Read(buffer)
	if count > 0 {
		body.observer.Phase("ambiguous")
	}
	return count, err
}

func traceHTTPTransport(request *http.Request, observer *TransportObserver) *http.Request {
	if observer == nil {
		return request
	}
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			observer.update(func(diagnostic *UpstreamTransportDiagnostic) {
				diagnostic.ConnectionState = "connected"
				diagnostic.ConnectionReused = &info.Reused
				if info.Conn != nil {
					diagnostic.TCPPeer = socketPeer(info.Conn.RemoteAddr())
				}
			})
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				observer.Phase("after_payload")
			}
		},
	}
	cloned := request.Clone(httptrace.WithClientTrace(request.Context(), trace))
	if request.Method == http.MethodPost {
		cloned.GetBody = nil
	}
	if cloned.Body != nil && cloned.Body != http.NoBody {
		cloned.Body = &tracedRequestBody{ReadCloser: cloned.Body, observer: observer}
	}
	return cloned
}

func socketPeer(address net.Addr) string {
	if address == nil {
		return ""
	}
	return address.String()
}

func TransportPoolKeyHash(key string) string { return hashRiskIdentity(key) }

func transportDiagnosticJSON(diagnostic *UpstreamTransportDiagnostic) string {
	if diagnostic == nil {
		return ""
	}
	encoded, _ := json.Marshal(diagnostic)
	return string(encoded)
}
