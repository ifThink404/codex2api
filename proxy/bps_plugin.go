package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/upstreamprivacy"
	"github.com/codex2api/proxy/plugins"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// BPS transport plugin: routes opted-in Codex OAuth/AT accounts to the Basis
// Points endpoint (profiles word/excel/sheets/powerpoint). Enablement follows
// the framework precedence; codex_bps_enabled is the per-account override and
// openai_excel_bps (upstream's official Excel BPS switch) forces the plugin on
// with the Excel profile.

const BPSPluginID = "bps"

// BPSConfig is the plugin config JSON (transport_plugins.config). Zero values
// mean the documented defaults.
type BPSConfig struct {
	WordUserAgent                   string `json:"word_user_agent,omitempty"`
	RoundConvergenceLimit           int    `json:"round_convergence_limit,omitempty"`
	RoundTaskLifetimeHours          int    `json:"round_task_lifetime_hours,omitempty"`
	TurnTaskLifetimeHours           int    `json:"turn_task_lifetime_hours,omitempty"`
	TurnRoundLimit                  int    `json:"turn_round_limit,omitempty"`
	AttachmentRequestConcurrency    int    `json:"attachment_request_concurrency,omitempty"`
	AttachmentInstanceConcurrency   int    `json:"attachment_instance_concurrency,omitempty"`
	AttachmentAccountConcurrency    int    `json:"attachment_account_concurrency,omitempty"`
	Attachment429Fallback           bool   `json:"attachment_429_fallback,omitempty"`
	// ExcludeFailuresFromNativeHealth keeps BPS failures out of native
	// account health and cooldown. Absent means on, matching upstream's
	// official BPS, which never reports its provider failures.
	ExcludeFailuresFromNativeHealth *bool `json:"exclude_failures_from_native_health,omitempty"`
}

// SparesNativeHealth reports whether BPS failures stay out of native account
// health and cooldown.
func (c BPSConfig) SparesNativeHealth() bool {
	return c.ExcludeFailuresFromNativeHealth == nil || *c.ExcludeFailuresFromNativeHealth
}

func (c BPSConfig) normalized() BPSConfig {
	c.WordUserAgent = strings.TrimSpace(c.WordUserAgent)
	c.RoundConvergenceLimit = database.NormalizeBPSRoundConvergenceLimit(c.RoundConvergenceLimit)
	c.RoundTaskLifetimeHours = database.NormalizeBPSRoundTaskLifetimeHours(c.RoundTaskLifetimeHours)
	c.TurnTaskLifetimeHours = database.NormalizeBPSTurnTaskLifetimeHours(c.TurnTaskLifetimeHours)
	c.TurnRoundLimit = database.NormalizeBPSTurnRoundLimit(c.TurnRoundLimit)
	c.AttachmentRequestConcurrency = database.NormalizeBPSAttachmentRequestConcurrency(c.AttachmentRequestConcurrency)
	c.AttachmentInstanceConcurrency = database.NormalizeBPSAttachmentInstanceConcurrency(c.AttachmentInstanceConcurrency)
	c.AttachmentAccountConcurrency = database.NormalizeBPSAttachmentAccountConcurrency(c.AttachmentAccountConcurrency)
	return c
}

func parseBPSConfig(raw json.RawMessage) (BPSConfig, error) {
	var cfg BPSConfig
	if len(bytes.TrimSpace(raw)) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&cfg); err != nil {
			return BPSConfig{}, fmt.Errorf("invalid BPS config: %w", err)
		}
	}
	if cfg.WordUserAgent != "" && (len(cfg.WordUserAgent) > 2048 || strings.ContainsAny(cfg.WordUserAgent, "\r\n\x00")) {
		return BPSConfig{}, fmt.Errorf("word_user_agent must be a single header value of at most 2048 bytes")
	}
	return cfg.normalized(), nil
}

var bpsConfigSnapshot atomic.Pointer[BPSConfig]

// currentBPSConfig is the active plugin config; each registry reload of the
// BPS state republishes it.
func currentBPSConfig() BPSConfig {
	if cfg := bpsConfigSnapshot.Load(); cfg != nil {
		return *cfg
	}
	return BPSConfig{}.normalized()
}

func publishBPSConfig(raw json.RawMessage) {
	cfg, err := parseBPSConfig(raw)
	if err != nil {
		cfg = BPSConfig{}.normalized()
	}
	storeBPSConfig(cfg)
}

func storeBPSConfig(cfg BPSConfig) {
	cfg = cfg.normalized()
	bpsConfigSnapshot.Store(&cfg)
	// A raised upload limit must admit queued uploads immediately.
	bpsUploads.mu.Lock()
	bpsUploads.dispatch()
	bpsUploads.mu.Unlock()
}

// bpsIdentityStoreKey carries the durable BPS identity store (the database).
type bpsIdentityStoreKey struct{}

type bpsPlugin struct{}

func init() { plugins.Register(bpsPlugin{}) }

func (bpsPlugin) ID() string { return BPSPluginID }

func (bpsPlugin) Describe() plugins.Meta {
	return plugins.Meta{
		Name:                  "BPS",
		Description:           "Basis Points transport for Codex OAuth accounts (Word, Excel, Sheets, PowerPoint profiles).",
		Kinds:                 []plugins.RequestKind{plugins.KindResponses, plugins.KindResponsesCompact, plugins.KindChatCompletions, plugins.KindMessages},
		OverrideCredentialKey: auth.CodexBPSEnabledCredentialKey,
		UpstreamEndpoint:      CodexBPSBaseURL + "/responses",
	}
}

func (bpsPlugin) ValidateConfig(raw json.RawMessage) error {
	_, err := parseBPSConfig(raw)
	return err
}

// SparesNativeHealth follows exclude_failures_from_native_health.
func (bpsPlugin) SparesNativeHealth() bool { return currentBPSConfig().SparesNativeHealth() }

func (bpsPlugin) Migrate(ctx context.Context, db *database.DB) error {
	return db.MigrateBPSPlugin(ctx)
}

func (bpsPlugin) StateChanged(state database.TransportPluginState) {
	publishBPSConfig(state.Config)
}

// ForcedFor: upstream's openai_excel_bps switch means "BPS plugin, Excel
// profile" for eligible accounts.
func (bpsPlugin) ForcedFor(account *auth.Account) bool {
	return upstreamExcelBPSFlag(account)
}

func upstreamExcelBPSFlag(account *auth.Account) bool {
	if !account.CodexBPSEligible() {
		return false
	}
	account.Mu().RLock()
	defer account.Mu().RUnlock()
	return account.ExcelBPSEnabled
}

// upstreamExcelBPSActive replaces upstream's IsExcelBPSAvailableForModel at
// its handler branch sites: the BPS plugin owns Excel BPS, so upstream's own
// adapter branch never runs.
func upstreamExcelBPSActive(*auth.Account, string) bool { return false }

// bpsServesAccount reports whether BPS would serve a main-turn request for
// model on this account: plugin enabled plus the per-account route switches.
func bpsServesAccount(account *auth.Account, model string) bool {
	if account == nil {
		return false
	}
	p, _ := plugins.Default().Get(BPSPluginID)
	if p == nil {
		return false
	}
	return account.CodexRouteAllows("bps", model, false, plugins.Default().EnabledFor(p, account))
}

// bpsRequest is the plugin's per-inbound-request state.
type bpsRequest struct {
	handler  *Handler
	c        *gin.Context
	compact  bool
	related  bool
	pinned   bool
	inferred *inferredBPSSession
	upload   *bpsUploadRequest
}

func bpsRequestState(req *plugins.Request) *bpsRequest {
	if req == nil {
		return nil
	}
	state, _ := req.State(BPSPluginID).(*bpsRequest)
	return state
}

func (bpsPlugin) BindRequest(req *plugins.Request) {
	host := transportPluginHostOf(req)
	if host == nil || host.c == nil || host.c.Request == nil {
		return
	}
	c := host.c
	state := &bpsRequest{handler: host.handler, c: c, compact: req.Kind == plugins.KindResponsesCompact}
	state.related = codexRouteIsAuxiliary(req.Body) || resolveRequestRootSessionIdentity(req.Header, req.Body).related
	state.pinned = host.handler.bpsStickyDomain(c.Request.Context(), req)
	identity := resolveRequestSessionIdentity(req.Header, req.Body)
	root := resolveRequestRootSessionIdentity(req.Header, req.Body)
	c.Request = c.Request.WithContext(withBPSCaller(c, c.Request.Context()))
	state.inferred = bindInferredBPSSession(c, req.Body, identity, root)
	state.upload = host.handler.newBPSUploadRequest(c, req.Body, state.compact)
	req.SetState(BPSPluginID, state)
}

func codexRouteIsAuxiliary(body []byte) bool {
	meta := diagnosticMetadataObject(gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata"))
	kind := strings.ToLower(meta.Get("request_kind").String())
	return kind == "compaction" || requestBodyHasCompactionTrigger(body)
}

// Pinned: a continuation of a BPS-produced response or BPS compaction state
// must stay on BPS; opaque encrypted content is bound to its upstream.
func (bpsPlugin) Pinned(_ context.Context, req *plugins.Request) bool {
	state := bpsRequestState(req)
	return state != nil && state.pinned
}

// Admissible vetoes accounts during an active BPS upload cooldown for the
// request's inline attachments, and accounts whose BPS route excludes the
// model on a pinned request.
func (bpsPlugin) Admissible(ctx context.Context, account *auth.Account, model string) (bool, string) {
	req := plugins.RequestFromContext(ctx)
	state := bpsRequestState(req)
	if state == nil {
		return true, ""
	}
	if state.pinned && !account.CodexRouteAllows("bps", model, state.related, true) {
		return false, "bps_route_unavailable"
	}
	if account.CodexRouteAllows("bps", model, state.related, true) && bpsUploadCooldownForAccount(context.WithValue(ctx, bpsUploadRequestKey{}, state.upload), account) {
		return false, bpsUploadCooldownReason
	}
	return true, ""
}

// Select: BPS serves the attempt when the account's BPS route allows the
// model, or the request is pinned to BPS. Native keeps serving accounts whose
// native route is still on unless the conversation is pinned.
func (bpsPlugin) Select(ctx context.Context, attempt plugins.Attempt) bool {
	state := bpsRequestState(attempt.Request)
	related := state != nil && state.related
	if mode, _ := ctx.Value(codexTestModeKey{}).(string); mode == "bps" || mode == "codex" {
		return mode == "bps"
	}
	if !attempt.Account.CodexRouteAllows("bps", attempt.Model, related, true) {
		return false
	}
	if state != nil && state.pinned {
		return true
	}
	return !attempt.Account.CodexRouteAllows("native", attempt.Model, related, true)
}

func (bpsPlugin) PreferredAccount(ctx context.Context, req *plugins.Request, model string) int64 {
	state := bpsRequestState(req)
	if state == nil || state.handler == nil {
		return 0
	}
	return state.handler.bpsPreferredTaskAccount(ctx, state.inferred, model)
}

func (bpsPlugin) AccountSelected(ctx context.Context, req *plugins.Request, account *auth.Account, model string) {
	state := bpsRequestState(req)
	if state == nil || state.handler == nil || state.inferred == nil {
		return
	}
	if state.inferred.affinity == nil {
		state.inferred.affinity = &bpsTaskAffinityDiagnostic{TaskKey: state.inferred.seed, Result: "new_task", turnEpoch: NewUpstreamSessionUUID()}
	}
	state.handler.rememberBPSTaskAccount(ctx, state.inferred, account, model)
}

const bpsAttemptDiagnosticKey = "bps_diagnostic"

type bpsAttemptEnvKey struct{}

// BPSDiagnosticRecorder captures the latest BPS request diagnostic of the
// requests executed under its context (connection tests, diagnostics).
type BPSDiagnosticRecorder struct {
	mu   sync.Mutex
	last *CodexBPSDiagnostic
}

type bpsDiagnosticRecorderKey struct{}

// WithBPSDiagnosticRecorder attaches a recorder to ctx.
func WithBPSDiagnosticRecorder(ctx context.Context) (context.Context, *BPSDiagnosticRecorder) {
	recorder := &BPSDiagnosticRecorder{}
	return context.WithValue(ctx, bpsDiagnosticRecorderKey{}, recorder), recorder
}

// BPSDiagnosticFromContext returns the diagnostic recorded under ctx, if any.
func BPSDiagnosticFromContext(ctx context.Context) *CodexBPSDiagnostic {
	recorder, _ := ctx.Value(bpsDiagnosticRecorderKey{}).(*BPSDiagnosticRecorder)
	return recorder.Last()
}

// Last returns the most recent diagnostic.
func (r *BPSDiagnosticRecorder) Last() *CodexBPSDiagnostic {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// storeBPSAttemptDiagnostic hands the prepared request diagnostic to the
// attempt env (for the response transformer) and to any recorder.
func storeBPSAttemptDiagnostic(ctx context.Context, d *CodexBPSDiagnostic) {
	if env, _ := ctx.Value(bpsAttemptEnvKey{}).(*plugins.ReqEnv); env != nil {
		env.SetState(bpsAttemptDiagnosticKey, d)
	}
	if recorder, _ := ctx.Value(bpsDiagnosticRecorderKey{}).(*BPSDiagnosticRecorder); recorder != nil {
		recorder.mu.Lock()
		recorder.last = d
		recorder.mu.Unlock()
	}
}

func bpsAttemptDiagnostic(env *plugins.ReqEnv) *CodexBPSDiagnostic {
	if env == nil {
		return nil
	}
	d, _ := env.State(bpsAttemptDiagnosticKey).(*CodexBPSDiagnostic)
	return d
}

func (bpsPlugin) Execute(ctx context.Context, env *plugins.ReqEnv) (*http.Response, error) {
	state := bpsRequestState(env.Request)
	var handler *Handler
	if state != nil {
		handler = state.handler
		ctx = context.WithValue(ctx, inferredBPSSessionKey{}, state.inferred)
		ctx = withBPSCaller(state.c, ctx)
		if handler != nil {
			ctx = WithBPSAttachmentCache(ctx, handler.bpsCache())
			if handler.db != nil {
				ctx = context.WithValue(ctx, bpsIdentityStoreKey{}, handler.db)
			}
		}
	}
	ctx = withBPSTurnQuestionInput(ctx, env.Account, env.Header, env.Body)
	ctx = context.WithValue(ctx, bpsAttemptEnvKey{}, env)
	var deviceCfg *DeviceProfileConfig
	if handler != nil {
		deviceCfg = handler.deviceCfg
	}
	if deviceCfg == nil {
		deviceCfg = &DeviceProfileConfig{}
	}
	endpoint := CodexBPSBaseURL + "/responses"
	if env.Compact {
		endpoint += "/compact"
	}
	env.Request.SetUsageUpstreamEndpoint(BPSPluginID, endpoint)
	resp, err := executeCodexBPS(ctx, pluginServices(env), env.Account, env.Body, env.CacheKey, env.ProxyURL, env.APIKey, deviceCfg, env.Header, env.Compact)
	if err == nil && resp != nil && resp.StatusCode == http.StatusOK && !env.Compact && bpsStreamIsEventStream(resp.Header.Get("Content-Type")) {
		request := env.Request
		resp.Body = newBPSStreamGuard(resp.Body, func() { request.SetUsageErrorKind(BPSPluginID, BPSCutoffCompletedKind) })
	}
	if d := bpsAttemptDiagnostic(env); d != nil {
		meta := map[string]string{"profile": string(d.Profile)}
		if d.AgentIteration != "" {
			meta["agent_iteration"] = d.AgentIteration
		}
		encoded, _ := json.Marshal(meta)
		env.Request.SetUsageMeta(BPSPluginID, string(encoded))
	}
	return resp, err
}

// OnExecuteError records BPS attachment-preparation failures as one zero-token
// attempt and starts the account's upload cooldown on a 429.
func (bpsPlugin) OnExecuteError(_ context.Context, env *plugins.ReqEnv, err error) {
	state := bpsRequestState(env.Request)
	if state == nil || state.handler == nil || state.c == nil {
		return
	}
	state.handler.logBPSPreparationFailure(state.c, err, &database.UsageLogInput{
		AccountID: env.Account.ID(), Endpoint: bpsInboundEndpoint(env.Request.Kind), Model: env.Model, EffectiveModel: env.Model,
		AttemptIndex: env.Attempt,
	})
}

func bpsInboundEndpoint(kind plugins.RequestKind) string {
	switch kind {
	case plugins.KindResponsesCompact:
		return "/v1/responses/compact"
	case plugins.KindChatCompletions:
		return "/v1/chat/completions"
	case plugins.KindMessages:
		return "/v1/messages"
	}
	return "/v1/responses"
}

// Response transformer: projectBPSResponse on every JSON body and SSE frame
// (before usage extraction, so the fixed runtime-overhead billing applies to
// the extracted usage), provider error scrubbing, plus provenance recording
// for the sticky domain.

func (bpsPlugin) FilterHeaders(_ *plugins.ReqEnv, header http.Header) {
	for name := range header {
		if bpsSourceField(name) || strings.EqualFold(name, "X-Codex-Turn-State") {
			header.Del(name)
		}
	}
}

func bpsEnvProjectionContext(env *plugins.ReqEnv) context.Context {
	return context.WithValue(context.Background(), codexBPSDiagnosticKey{}, bpsAttemptDiagnostic(env))
}

func (bpsPlugin) TransformJSON(env *plugins.ReqEnv, status int, body []byte) ([]byte, error) {
	if status >= 400 {
		return scrubBPSErrorBody(status, body), nil
	}
	if bpsAttemptDiagnostic(env) == nil {
		return body, nil
	}
	projected, err := projectBPSResponse(bpsEnvProjectionContext(env), body)
	if err != nil {
		return nil, err
	}
	bpsRecordProvenance(env, projected)
	return upstreamprivacy.Bytes(projected), nil
}

func (bpsPlugin) TransformSSEFrame(env *plugins.ReqEnv, event string, data []byte) ([]plugins.SSEFrame, error) {
	d := bpsAttemptDiagnostic(env)
	if d == nil {
		return []plugins.SSEFrame{{Event: event, Data: data}}, nil
	}
	if d.Timing != nil {
		d.Timing.event(time.Now(), isFirstTokenResult(gjson.ParseBytes(data)))
	}
	projected, err := projectBPSResponse(bpsEnvProjectionContext(env), data)
	if err == nil {
		projected, err = scrubBPSTerminalEvent(projected)
	}
	if err != nil {
		return nil, err
	}
	bpsRecordProvenance(env, projected)
	return bpsAttemptRedactor(env).push(event, projected)
}

// FinishSSE releases frames the redactor still holds when the stream ends.
func (bpsPlugin) FinishSSE(env *plugins.ReqEnv) ([]plugins.SSEFrame, error) {
	if bpsAttemptDiagnostic(env) == nil {
		return nil, nil
	}
	return bpsAttemptRedactor(env).finish()
}

const bpsAttemptRedactorKey = "bps_stream_redactor"

func bpsAttemptRedactor(env *plugins.ReqEnv) *bpsStreamRedactor {
	if r, _ := env.State(bpsAttemptRedactorKey).(*bpsStreamRedactor); r != nil {
		return r
	}
	r := newBPSStreamRedactor()
	env.SetState(bpsAttemptRedactorKey, r)
	return r
}

// Sticky domain: response IDs and compaction contents BPS produced, keyed in
// the runtime cache so every replica routes their continuations back to BPS.

const (
	bpsProvenanceNamespace = "bps-provenance-v1"
	bpsProvenanceTTL       = 7 * 24 * time.Hour
	bpsProvenanceTimeout   = 300 * time.Millisecond
)

func bpsProvenanceResponseKey(id string) string { return codexIdentityDigest("bps-response", id) }

func bpsProvenanceContentKey(content string) string {
	return codexIdentityDigest("bps-compaction", compactionContentDigest(content))
}

func bpsRecordProvenance(env *plugins.ReqEnv, payload []byte) {
	state := bpsRequestState(env.Request)
	if state == nil || state.handler.bpsCache() == nil {
		return
	}
	var keys []string
	root := gjson.ParseBytes(payload)
	for _, path := range []string{"response.id", "id"} {
		if id := root.Get(path).String(); strings.HasPrefix(id, "resp") {
			if path == "id" && root.Get("object").String() != "response" && root.Get("object").String() != "response.compaction" {
				continue
			}
			keys = append(keys, bpsProvenanceResponseKey(id))
			break
		}
	}
	for _, content := range compactionEncryptedContentsFromPayload(payload) {
		keys = append(keys, bpsProvenanceContentKey(content))
	}
	if len(keys) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), bpsProvenanceTimeout)
	defer cancel()
	marker := json.RawMessage(`true`)
	for _, key := range keys {
		_ = state.handler.bpsCache().SetRuntime(ctx, bpsProvenanceNamespace, key, marker, bpsProvenanceTTL)
	}
}

func (h *Handler) bpsStickyDomain(ctx context.Context, req *plugins.Request) bool {
	store := h.bpsCache()
	if store == nil || req == nil {
		return false
	}
	var keys []string
	if id := strings.TrimSpace(gjson.GetBytes(req.Body, "previous_response_id").String()); id != "" {
		keys = append(keys, bpsProvenanceResponseKey(id))
	}
	for _, content := range requestCompactionEncryptedContents(req.Body) {
		keys = append(keys, bpsProvenanceContentKey(content))
	}
	if len(keys) == 0 {
		return false
	}
	lookupCtx, cancel := context.WithTimeout(ctx, bpsProvenanceTimeout)
	defer cancel()
	for _, key := range keys {
		if _, found, err := store.GetRuntime(lookupCtx, bpsProvenanceNamespace, key); err == nil && found {
			return true
		}
	}
	return false
}

// UpstreamExcelBPSActive is the exported form of upstreamExcelBPSActive for
// upstream's admin connection-test intercept sites.
func UpstreamExcelBPSActive(account *auth.Account, model string) bool {
	return upstreamExcelBPSActive(account, model)
}

// ExecuteCodexConnectionTest runs an account connection test on the transport
// ordinary traffic would use, or on the one an explicit test mode
// (WithCodexTestMode) selects; the mode never changes account settings.
func ExecuteCodexConnectionTest(ctx context.Context, account *auth.Account, payload []byte, proxyURL string) (*http.Response, error) {
	model := gjson.GetBytes(payload, "model").String()
	req := plugins.NewRequest(NewUpstreamSessionUUID(), plugins.KindResponses, payload, nil, 0)
	registry := plugins.Default()
	var route *plugins.Route
	if mode, _ := ctx.Value(codexTestModeKey{}).(string); mode == "bps" {
		if err := ValidateCodexTestMode(ctx, account); err != nil {
			return nil, bpsError(http.StatusBadRequest, "codex_test_mode_unsupported", "%s", err.Error())
		}
		if p, ok := registry.Get(BPSPluginID); ok {
			route = registry.RouteFor(p, req, account, model, plugins.KindResponses)
		}
	} else if account != nil && !account.IsRelayStyle() {
		route = registry.Resolve(ctx, req, account, model, plugins.KindResponses)
	}
	if route != nil {
		return route.Execute(ctx, plugins.ReqEnv{Account: account, Model: model, Body: payload, CacheKey: NewUpstreamSessionUUID(), ProxyURL: proxyURL})
	}
	return ExecuteRequest(ctx, account, payload, "", proxyURL, "", nil, nil)
}
