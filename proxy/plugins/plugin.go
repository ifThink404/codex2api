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
// the inbound request. BindRequest runs once per inbound request, before the
// first Select, for every plugin enabled for that attempt's account.
type RequestBinder interface {
	BindRequest(req *Request)
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
	// lines). Returning drop=true removes the frame from the stream.
	TransformSSEFrame(env *ReqEnv, event string, data []byte) (outEvent string, outData []byte, drop bool)
}

// ConfigValidator validates the plugin's config JSON object before it is saved.
type ConfigValidator interface {
	ValidateConfig(config json.RawMessage) error
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
}
