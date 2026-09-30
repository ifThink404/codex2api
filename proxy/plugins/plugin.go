// Package plugins is the transport plugin framework: compiled-in alternative
// upstream channels that can take over individual request attempts from the
// native Codex executor.
//
// Core wiring is deliberately thin. At each request site the proxy handler
// calls into this package exactly once per concern:
//
//   - Registry.AccountFilter wraps the scheduler account filter so a plugin can
//     veto accounts it currently cannot serve (Plugin.Admissible).
//   - Registry.Resolve runs once per attempt and returns the Route of the first
//     plugin that is enabled for the account and selects the attempt, or nil
//     for the native path.
//   - Route.Execute replaces the native executor. It performs the upstream call
//     through the plugin, records sampled captures and wraps the response so
//     ResponseTransformer runs before core reads headers, JSON or SSE frames
//     (and therefore before usage extraction).
//
// Enablement precedence for a (plugin, account) pair: the account's override
// credential (Meta.OverrideCredentialKey) > membership in one of the plugin's
// account groups > the plugin's global switch. Plugins default to OFF; state
// lives in the transport_plugins table and every replica hot-reloads it via
// the scheduler outbox 'plugin' entity.
package plugins

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

// RequestKind identifies the inbound endpoint family an attempt belongs to.
type RequestKind string

const (
	KindResponses        RequestKind = "responses"
	KindResponsesCompact RequestKind = "responses_compact"
	KindChatCompletions  RequestKind = "chat_completions"
	KindMessages         RequestKind = "messages"
)

// Meta describes a plugin to core and to the admin API.
type Meta struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Kinds lists the request kinds the plugin can serve. Resolve and the
	// account filter ignore the plugin for any other kind.
	Kinds []RequestKind `json:"kinds"`
	// OverrideCredentialKey is the account credential holding the per-account
	// override (JSON bool; absent/null = inherit). Empty selects
	// DefaultOverrideCredentialKey(ID). Must match [A-Za-z0-9_]+.
	OverrideCredentialKey string `json:"override_credential_key"`
	// UpstreamEndpoint, when set, replaces usage_logs.upstream_endpoint for
	// attempts this plugin served.
	UpstreamEndpoint string `json:"upstream_endpoint,omitempty"`
}

// Plugin is the contract every transport plugin implements. Implementations
// must be safe for concurrent use; per-request state belongs in Request.
type Plugin interface {
	// ID is the stable identifier ([a-z][a-z0-9_]{0,31}, not "native"). It is
	// the transport_plugins primary key and the usage_logs.transport value.
	ID() string
	Describe() Meta
	// Admissible vetoes an account for scheduling while the plugin is enabled
	// for it (for example an upload cooldown). reason is for diagnostics. It
	// runs on the scheduler hot path and must not block.
	Admissible(ctx context.Context, account *auth.Account, model string) (ok bool, reason string)
	// Select decides whether this plugin serves the attempt. It is only asked
	// when the plugin is enabled for the account. Sticky routing (e.g. a
	// previous_response_id the plugin produced) is decided here.
	Select(ctx context.Context, attempt Attempt) bool
	// Execute performs the upstream call and returns a Responses-shaped HTTP
	// response (JSON or SSE, as the native executor would for the same kind).
	// Non-200 responses flow into core's normal retry/cooldown handling.
	Execute(ctx context.Context, env *ReqEnv) (*http.Response, error)
}

// RequestBinder is implemented by plugins that derive per-request state from
// the inbound request. BindRequest runs once per inbound request, before
// account selection, for every registered plugin supporting the request kind.
type RequestBinder interface {
	BindRequest(req *Request)
}

// SSEFrame is one server-sent event.
type SSEFrame struct {
	Event string
	Data  []byte
}

// ResponseTransformer rewrites plugin responses into the shape core expects.
// Core applies it to every response Route.Execute returns, before reading the
// body, so usage extraction sees transformed data.
type ResponseTransformer interface {
	// FilterHeaders edits response headers in place.
	FilterHeaders(env *ReqEnv, header http.Header)
	// TransformJSON rewrites a non-SSE body.
	TransformJSON(env *ReqEnv, status int, body []byte) ([]byte, error)
	// TransformSSEFrame rewrites one SSE frame (event name and joined data
	// lines) into zero or more frames, emitted in order. Returning exactly the
	// input frame passes the original bytes through. A transformer may hold
	// data back across frames (per-attempt state lives in env) and release it
	// later or from FinishSSE.
	TransformSSEFrame(env *ReqEnv, event string, data []byte) ([]SSEFrame, error)
}

// SSEFinisher is implemented by transformers that buffer across frames; the
// frames it returns are appended when the upstream stream ends.
type SSEFinisher interface {
	FinishSSE(env *ReqEnv) ([]SSEFrame, error)
}

// Pinner is implemented by plugins whose produced state is route-bound (for
// example opaque encrypted content only the plugin's upstream accepts). A
// pinned request may only be scheduled on accounts the plugin is enabled for
// and admits; Select is still asked per attempt.
type Pinner interface {
	Pinned(ctx context.Context, req *Request) bool
}

// AccountPreferrer supplies a soft scheduling preference (an account ID to
// try first) and learns the account that was finally selected. It never
// overrides filters, exclusions or capacity.
type AccountPreferrer interface {
	PreferredAccount(ctx context.Context, req *Request, model string) int64
	AccountSelected(ctx context.Context, req *Request, account *auth.Account, model string)
}

// SameAccountRetrier is optionally implemented by plugins that can hand a
// failed attempt back to the same account on another transport (e.g. BPS
// refusing the account, whose native route then serves the retry). Core
// lifts that account's retry exclusion for the next selection, once;
// PreferredAccount should then offer it.
type SameAccountRetrier interface {
	RetryAccount(ctx context.Context, req *Request) int64
}

// ExecuteErrorHandler is told about every error Execute returns, while the
// attempt's account and trace are still current (e.g. to log a zero-token
// preparation failure or start an upload cooldown).
type ExecuteErrorHandler interface {
	OnExecuteError(ctx context.Context, env *ReqEnv, err error)
}

// AccountPreferenceFilter is optionally implemented by plugins that want
// scheduling to try a subset of accounts first for a request (e.g. accounts
// able to serve a model only the plugin can serve). nil means no preference.
// Core falls back to every admissible account when no preferred one is free.
type AccountPreferenceFilter interface {
	PreferredAccounts(ctx context.Context, req *Request, model string) func(*auth.Account) bool
}

// Maintainer is optionally implemented by plugins that own data needing
// periodic cleanup; the capture retention job calls it every 10 minutes.
type Maintainer interface {
	Maintain(ctx context.Context, db *database.DB, now time.Time) error
}

// NativeHealthPolicy is optionally implemented by plugins whose failures
// should stay out of the native account health score and cooldowns. Core
// still reports account-level signals (revoked credentials, deactivated
// accounts), which describe the account rather than the transport.
type NativeHealthPolicy interface {
	SparesNativeHealth() bool
}

// ConfigValidator validates the plugin's config JSON object before it is saved.
type ConfigValidator interface {
	ValidateConfig(config json.RawMessage) error
}

// StateObserver is told about every state snapshot the registry publishes
// (startup, admin saves and outbox hot reloads), including default state when
// no row exists yet.
type StateObserver interface {
	StateChanged(state database.TransportPluginState)
}

// Migrator is implemented by plugins that own database tables. Registry.Attach
// runs it once at startup.
type Migrator interface {
	Migrate(ctx context.Context, db *database.DB) error
}

// DefaultOverrideCredentialKey is the per-account override credential used
// when Meta.OverrideCredentialKey is empty.
func DefaultOverrideCredentialKey(pluginID string) string {
	return "transport_plugin_" + pluginID + "_enabled"
}

// Services is the core plumbing a plugin must use for its upstream calls so
// they are routed, audited and billed exactly like native requests. Core
// installs it once (Registry.SetServices); Route.Execute puts it on ReqEnv.
type Services interface {
	// HTTPClient returns the pooled client for account/proxyURL (the Resin
	// client when Resin is enabled) and the rewrite core applies to upstream
	// URLs (Resin reverse proxy; identity otherwise).
	HTTPClient(account *auth.Account, proxyURL string) (*http.Client, func(url string) string)
	// PrepareRequest applies core's per-account request decoration (the Resin
	// account header) to an upstream request.
	PrepareRequest(req *http.Request, account *auth.Account)
	// Do sends req and records the upstream trace (request ID, proxy label)
	// against the attempt's account, like the native executor.
	Do(client *http.Client, req *http.Request, account *auth.Account, proxyURL string) (*http.Response, error)
	// RecordUserAgent records the final outbound User-Agent for usage logs.
	RecordUserAgent(ctx context.Context, userAgent string)
	// ConsumeModelQuota charges the API key's per-model request quota; call
	// it once per upstream inference request, before sending.
	ConsumeModelQuota(ctx context.Context, model string) error
	// RecycleClient drops a pooled client after a transport error that
	// poisons connections.
	RecycleClient(account *auth.Account, proxyURL string, err error)
}

// Attempt is what Select sees for one upstream attempt.
type Attempt struct {
	Request *Request
	Account *auth.Account
	Model   string
	Kind    RequestKind
	// Index is 1-based within the inbound request.
	Index int
	// Prior is the transport that served the previous attempt of this request
	// ("" on the first attempt, database.TransportNative for native).
	Prior string
}

// ReqEnv is the execution environment core hands to Plugin.Execute.
type ReqEnv struct {
	Request *Request
	Account *auth.Account
	Model   string
	// Body is the fully prepared upstream Responses body (mapping, payload
	// rules and turn-state policy already applied).
	Body []byte
	// Header is a clone of the downstream request headers.
	Header   http.Header
	CacheKey string
	ProxyURL string
	// APIKey is the downstream bearer token (for device-profile stabilization).
	APIKey  string
	Compact bool
	Attempt int
	// Config is the plugin's config object from the active snapshot.
	Config json.RawMessage
	// Services is the core plumbing for upstream calls (see Services).
	Services Services

	state map[string]any
	// upstreamCapture records the outbound upstream request (see
	// CaptureUpstreamRequest); nil when the attempt is not sampled.
	upstreamCapture func(header http.Header, body []byte)
	// captureClassify marks the attempt's captures as errors (see
	// ClassifyCapture); nil when the attempt is not sampled.
	captureClassify func(kind string)
}

// ClassifyCapture marks this attempt's captures still to be written (the
// response capture) with an error kind, for failures only the plugin can see,
// such as a usage-policy block inside a 200 stream. Error captures follow the
// longer error retention window.
func (env *ReqEnv) ClassifyCapture(kind string) {
	if env != nil && env.captureClassify != nil {
		env.captureClassify(kind)
	}
}

// CaptureUpstreamRequest records the request the plugin actually sends
// upstream, linked to this attempt, when the attempt is sampled for capture.
func (env *ReqEnv) CaptureUpstreamRequest(header http.Header, body []byte) {
	if env != nil && env.upstreamCapture != nil {
		env.upstreamCapture(header, body)
	}
}

// State returns per-attempt plugin state stored with SetState.
func (env *ReqEnv) State(key string) any { return env.state[key] }

// SetState stores per-attempt plugin state (e.g. stream buffers).
func (env *ReqEnv) SetState(key string, value any) {
	if env.state == nil {
		env.state = map[string]any{}
	}
	env.state[key] = value
}
