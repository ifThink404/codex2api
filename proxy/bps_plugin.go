package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/upstreamprivacy"
	"github.com/codex2api/proxy/degradejudge"
	"github.com/codex2api/proxy/plugins"
	"github.com/codex2api/security"
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
	WordUserAgent                 string `json:"word_user_agent,omitempty"`
	RoundConvergenceLimit         int    `json:"round_convergence_limit,omitempty"`
	RoundTaskLifetimeHours        int    `json:"round_task_lifetime_hours,omitempty"`
	TurnTaskLifetimeHours         int    `json:"turn_task_lifetime_hours,omitempty"`
	TurnRoundLimit                int    `json:"turn_round_limit,omitempty"`
	AttachmentRequestConcurrency  int    `json:"attachment_request_concurrency,omitempty"`
	AttachmentInstanceConcurrency int    `json:"attachment_instance_concurrency,omitempty"`
	AttachmentAccountConcurrency  int    `json:"attachment_account_concurrency,omitempty"`
	Attachment429Fallback         bool   `json:"attachment_429_fallback,omitempty"`
	// ExcludeFailuresFromNativeHealth keeps BPS failures out of native
	// account health and cooldown. Absent means on, matching upstream's
	// official BPS, which never reports its provider failures.
	// BPSModels are the models BPS serves (globs; default gpt-5.6-*, gpt-6-*).
	// Other models are served natively. BPSOnlyModels are models native
	// cannot serve (default gpt-6-*): scheduling prefers BPS-capable accounts
	// for them instead of trying native first.
	BPSModels     []string `json:"bps_models,omitempty"`
	BPSOnlyModels []string `json:"bps_only_models,omitempty"`
	// PolicyBlockThreshold usage-policy blocks within 10 minutes (default 3)
	// trigger a BPS cooldown of the account. Without an explicit upstream
	// hint the cooldown climbs PolicyCooldownLadder one tier per trigger
	// (default 2m, 10m, 30m, 2h); a tier is forgiven per 2h without a block
	// and all of them after 24h.
	PolicyBlockThreshold            int      `json:"policy_block_threshold,omitempty"`
	PolicyCooldownLadder            []string `json:"bps_policy_cooldown_ladder,omitempty"`
	ExcludeFailuresFromNativeHealth *bool    `json:"exclude_failures_from_native_health,omitempty"`
	// ImageTrimDefault is codex_bps_image_trim_enabled for accounts without
	// an explicit value. Absent means on, as in fj-server.
	ImageTrimDefault *bool `json:"image_trim_default,omitempty"`
	// PersistHeuristicAffinity stores the task affinity of heuristic
	// (conversation-prefix) seeds in the database, shared by every replica.
	// Absent means on, as in fj-server; off keeps them in a local LRU.
	PersistHeuristicAffinity *bool `json:"persist_heuristic_affinity,omitempty"`
	// PolicyConversationMark marks the conversation of a usage-policy 403 so
	// its later turns avoid BPS for 30 minutes. Off by default: the block is
	// an account-level request quota, not a content verdict.
	PolicyConversationMark bool `json:"bps_policy_conversation_mark,omitempty"`
	// ProbeModel is the model of the background usage-policy probe; empty
	// uses the connection-test model when BPS serves it, else gpt-6-sol.
	ProbeModel string `json:"bps_probe_model,omitempty"`
	// MinUsableAccounts is the dashboard's warning floor for usable BPS
	// accounts (0 = the default, 2).
	MinUsableAccounts int `json:"bps_min_usable_accounts,omitempty"`
	// AccountMaxConcurrency caps BPS requests in flight per account on each
	// replica (0 = off).
	AccountMaxConcurrency int `json:"bps_account_max_concurrency,omitempty"`
	// AccountRequestBudget caps successful BPS requests per account over
	// AccountBudgetWindow (a duration, default 24h), across replicas (0 = off).
	AccountRequestBudget int    `json:"bps_account_request_budget,omitempty"`
	AccountBudgetWindow  string `json:"bps_account_budget_window,omitempty"`
	// Capture retention windows in hours (read by the plugin framework, see
	// plugins.ParseCaptureRetention): other captures default 6 (1-12), error
	// captures default and at most 12.
	CaptureRetentionHours      int `json:"capture_retention_hours,omitempty"`
	CaptureErrorRetentionHours int `json:"capture_error_retention_hours,omitempty"`
	// Degradation ("降智") breaker, per route of an account (see
	// route_breaker.go). A pelican judge probe scoring below
	// DegradeScoreThreshold (default 187, calibrated for gpt-6-astra at
	// medium effort), or a native 403, breaks that route; an upstream model
	// mismatch only schedules a confirming probe. DegradeProbeModel defaults
	// to gpt-6-astra; DegradeProbeInterval enables periodic sampling ("" =
	// off); DegradeProbeMaxConcurrent caps probes in flight per replica
	// (default 2); the cooldown climbs DegradeCooldownLadder (default: the
	// policy ladder). DegradeBreakerEnabled absent means on.
	DegradeBreakerEnabled     *bool    `json:"degrade_breaker_enabled,omitempty"`
	DegradeScoreThreshold     int      `json:"degrade_score_threshold,omitempty"`
	DegradeProbeModel         string   `json:"degrade_probe_model,omitempty"`
	DegradeProbeInterval      string   `json:"degrade_probe_interval,omitempty"`
	DegradeProbeMaxConcurrent int      `json:"degrade_probe_max_concurrent,omitempty"`
	DegradeCooldownLadder     []string `json:"degrade_cooldown_ladder,omitempty"`
	// DualRoutePreference picks the route a dual-route account (BPS and an
	// explicit native route) uses while both are healthy: native (default,
	// as in fj-server) or bps. The other route is the fallback.
	DualRoutePreference string `json:"dual_route_preference,omitempty"`
}

// Dual-route preferences.
const (
	DualRouteNative = "native"
	DualRouteBPS    = "bps"
)

// PrefersBPS reports whether dual-route accounts use BPS first.
func (c BPSConfig) PrefersBPS() bool { return c.DualRoutePreference == DualRouteBPS }

// DegradeEnabled reports whether the degradation breaker is on.
func (c BPSConfig) DegradeEnabled() bool {
	return c.DegradeBreakerEnabled == nil || *c.DegradeBreakerEnabled
}

// DegradeThreshold is the pelican score at or above which a route is fine.
func (c BPSConfig) DegradeThreshold() int {
	if c.DegradeScoreThreshold <= 0 {
		return degradejudge.DefaultThreshold
	}
	return c.DegradeScoreThreshold
}

// DegradeModel is the model the pelican probe asks for.
func (c BPSConfig) DegradeModel() string {
	if model := strings.TrimSpace(c.DegradeProbeModel); model != "" {
		return model
	}
	return degradejudge.DefaultModel
}

// DegradeInterval is the periodic sampling interval (0 = off).
func (c BPSConfig) DegradeInterval() time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(c.DegradeProbeInterval))
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// DegradeMaxConcurrent caps pelican probes in flight per replica.
func (c BPSConfig) DegradeMaxConcurrent() int {
	if c.DegradeProbeMaxConcurrent <= 0 {
		return 2
	}
	return c.DegradeProbeMaxConcurrent
}

// DegradeLadder is the degradation breaker cooldown per tier.
func (c BPSConfig) DegradeLadder() []time.Duration {
	if len(c.DegradeCooldownLadder) == 0 {
		return c.PolicyLadder()
	}
	return BPSConfig{PolicyCooldownLadder: c.DegradeCooldownLadder}.PolicyLadder()
}

// PersistsHeuristicAffinity reports whether heuristic seeds are bound in the
// database.
func (c BPSConfig) PersistsHeuristicAffinity() bool {
	return c.PersistHeuristicAffinity == nil || *c.PersistHeuristicAffinity
}

var defaultBPSPolicyCooldownLadder = []string{"2m", "10m", "30m", "2h"}

// PolicyLadder is the usage-policy cooldown per tier.
func (c BPSConfig) PolicyLadder() []time.Duration {
	tiers := c.PolicyCooldownLadder
	if len(tiers) == 0 {
		tiers = defaultBPSPolicyCooldownLadder
	}
	ladder := make([]time.Duration, 0, len(tiers))
	for _, tier := range tiers {
		if d, err := time.ParseDuration(strings.TrimSpace(tier)); err == nil && d > 0 {
			ladder = append(ladder, d)
		}
	}
	if len(ladder) == 0 {
		ladder = []time.Duration{2 * time.Minute, 10 * time.Minute, 30 * time.Minute, 2 * time.Hour}
	}
	return ladder
}

var (
	defaultBPSModels     = []string{"gpt-5.6-*", "gpt-6-*"}
	defaultBPSOnlyModels = []string{"gpt-6-*"}
)

// bpsModelMatches reports whether model matches one of the globs.
func bpsModelMatches(patterns []string, model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	for _, pattern := range patterns {
		if ok, _ := path.Match(strings.ToLower(strings.TrimSpace(pattern)), model); ok {
			return true
		}
	}
	return false
}

// ServesModel reports whether bps_models lets BPS serve model.
func (c BPSConfig) ServesModel(model string) bool {
	patterns := c.BPSModels
	if len(patterns) == 0 {
		patterns = defaultBPSModels
	}
	return bpsModelMatches(patterns, bpsWireModel(model))
}

// bpsWireModel is the model BPS is sent for a requested model
// (prepareCodexBPSBodyForProfile maps codex-auto-review).
func bpsWireModel(model string) string {
	if strings.EqualFold(strings.TrimSpace(model), "codex-auto-review") {
		return "gpt-5.6-luna"
	}
	return model
}

// BPSOnlyModel reports whether model is in bps_only_models.
func (c BPSConfig) BPSOnlyModel(model string) bool {
	patterns := c.BPSOnlyModels
	if len(patterns) == 0 {
		patterns = defaultBPSOnlyModels
	}
	return bpsModelMatches(patterns, model)
}

// TrimsImagesByDefault reports image_trim_default.
func (c BPSConfig) TrimsImagesByDefault() bool {
	return c.ImageTrimDefault == nil || *c.ImageTrimDefault
}

// bpsImageTrimEnabled resolves an account's history trim: its explicit
// setting, else the plugin default.
func bpsImageTrimEnabled(account *auth.Account) bool {
	if enabled, ok := account.CodexBPSImageTrimOverride(); ok {
		return enabled
	}
	return currentBPSConfig().TrimsImagesByDefault()
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
	if c.PolicyBlockThreshold < 1 || c.PolicyBlockThreshold > 100 {
		c.PolicyBlockThreshold = 3
	}

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
	for _, list := range [][]string{cfg.BPSModels, cfg.BPSOnlyModels} {
		if len(list) > 64 {
			return BPSConfig{}, fmt.Errorf("model lists hold at most 64 patterns")
		}
		for _, pattern := range list {
			if _, err := path.Match(strings.ToLower(strings.TrimSpace(pattern)), ""); strings.TrimSpace(pattern) == "" || len(pattern) > 128 || err != nil {
				return BPSConfig{}, fmt.Errorf("invalid model pattern %q", pattern)
			}
		}
	}
	if cfg.MinUsableAccounts < 0 || cfg.MinUsableAccounts > 1000 {
		return BPSConfig{}, fmt.Errorf("bps_min_usable_accounts must be between 0 (default) and 1000")
	}
	if cfg.AccountMaxConcurrency < 0 || cfg.AccountMaxConcurrency > 100 {
		return BPSConfig{}, fmt.Errorf("bps_account_max_concurrency must be between 0 (off) and 100")
	}
	if cfg.AccountRequestBudget < 0 || cfg.AccountRequestBudget > 10_000_000 {
		return BPSConfig{}, fmt.Errorf("bps_account_request_budget must be between 0 (off) and 10000000")
	}
	if window := strings.TrimSpace(cfg.AccountBudgetWindow); window != "" {
		if d, err := time.ParseDuration(window); err != nil || d < time.Hour || d > 30*24*time.Hour {
			return BPSConfig{}, fmt.Errorf("invalid bps_account_budget_window %q (use a duration such as 24h, 1h to 720h)", cfg.AccountBudgetWindow)
		}
	}
	for name, ladder := range map[string][]string{"bps_policy_cooldown_ladder": cfg.PolicyCooldownLadder, "degrade_cooldown_ladder": cfg.DegradeCooldownLadder} {
		if len(ladder) > 10 {
			return BPSConfig{}, fmt.Errorf("%s holds at most 10 tiers", name)
		}
		for _, tier := range ladder {
			if d, err := time.ParseDuration(strings.TrimSpace(tier)); err != nil || d < time.Second || d > 24*time.Hour {
				return BPSConfig{}, fmt.Errorf("invalid %s tier %q (use durations such as 2m or 2h, 1s to 24h)", name, tier)
			}
		}
	}
	switch cfg.DualRoutePreference {
	case "", DualRouteNative, DualRouteBPS:
	default:
		return BPSConfig{}, fmt.Errorf("dual_route_preference must be native or bps")
	}
	if cfg.DegradeScoreThreshold < 0 || cfg.DegradeScoreThreshold > 10000 {
		return BPSConfig{}, fmt.Errorf("degrade_score_threshold must be between 0 (default 187) and 10000")
	}
	if cfg.DegradeProbeMaxConcurrent < 0 || cfg.DegradeProbeMaxConcurrent > 16 {
		return BPSConfig{}, fmt.Errorf("degrade_probe_max_concurrent must be between 0 (default 2) and 16")
	}
	if len(cfg.DegradeProbeModel) > 128 || strings.ContainsAny(cfg.DegradeProbeModel, " \r\n") {
		return BPSConfig{}, fmt.Errorf("invalid degrade_probe_model %q", cfg.DegradeProbeModel)
	}
	if interval := strings.TrimSpace(cfg.DegradeProbeInterval); interval != "" {
		if d, err := time.ParseDuration(interval); err != nil || d < 10*time.Minute || d > 7*24*time.Hour {
			return BPSConfig{}, fmt.Errorf("invalid degrade_probe_interval %q (empty = off, or a duration such as 6h, 10m to 168h)", cfg.DegradeProbeInterval)
		}
	}
	if _, err := plugins.ParseCaptureRetention(raw); err != nil {
		return BPSConfig{}, err
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

// bpsIdentityRetention is how long an untouched identity row is kept: the
// longest configured round/turn task lifetime, and at least 30 days.
func bpsIdentityRetention(cfg BPSConfig) time.Duration {
	retention := database.BPSIdentityMinRetention
	for _, hours := range []int{cfg.RoundTaskLifetimeHours, cfg.TurnTaskLifetimeHours} {
		retention = max(retention, time.Duration(hours)*time.Hour)
	}
	return retention
}

// Maintain prunes BPS identity rows untouched for bpsIdentityRetention.
func (bpsPlugin) Maintain(ctx context.Context, db *database.DB, now time.Time) error {
	retention := bpsIdentityRetention(currentBPSConfig())
	deleted, err := db.PruneBPSIdentity(ctx, now.Add(-retention))
	if deleted > 0 {
		log.Printf("[bps] pruned %d identity rows untouched for %s", deleted, retention)
	}
	if blocks, pruneErr := db.PruneBPSPolicyBlocks(ctx, now.Add(-database.BPSPolicyBlockRetention)); pruneErr != nil {
		err = errors.Join(err, pruneErr)
	} else if blocks > 0 {
		log.Printf("[bps] pruned %d policy-block history rows older than %s", blocks, database.BPSPolicyBlockRetention)
	}
	if probes, pruneErr := db.PruneDegradeProbes(ctx, now.Add(-database.DegradeProbeRetention)); pruneErr != nil {
		err = errors.Join(err, pruneErr)
	} else if probes > 0 {
		log.Printf("[bps] pruned %d degrade probes older than %s", probes, database.DegradeProbeRetention)
	}
	return err
}

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

// upstreamExcelBPSActive is upstream's Excel Basispoints gate
// (excelBPSRouteAvailable) at the admin test sites: it never opens, because
// the BPS plugin owns Excel BPS.
func upstreamExcelBPSActive(account *auth.Account, model string) bool {
	return excelBPSRouteAvailable(account, model)
}

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
	return plugins.Default().EnabledFor(p, account) && bpsRouteAllows(context.Background(), nil, account, model, false)
}

// BPSOwnsAccount reports whether native inference is off limits for account:
// the BPS plugin is enabled for it and its native route is not explicitly on.
// Background jobs (usage probes, 5h-window activation, plan sync, model
// probes, detectors) must use the BPS transport for such an account, or skip
// it; native Codex traffic gets accounts marked as degraded upstream.
func BPSOwnsAccount(account *auth.Account) bool {
	if account == nil || !account.CodexBPSEligible() {
		return false
	}
	// A dual-route account whose native route breaker is open is owned by
	// BPS until the breaker's own native probe closes it.
	if account.CodexNativeRouteExplicit() && !nativeRouteOpen(context.Background(), nil, account) {
		return false
	}
	p, ok := plugins.Default().Get(BPSPluginID)
	return ok && plugins.Default().EnabledFor(p, account)
}

// bpsRouteAllows reports whether BPS may serve model on account: the
// account's BPS route, the plugin's bps_models, and no recent refusal of that
// model by BPS for this account. Related (auxiliary) requests follow their
// conversation regardless of the model lists.
func bpsRouteAllows(ctx context.Context, store cache.TokenCache, account *auth.Account, model string, related bool) bool {
	if !account.CodexRouteAllows("bps", model, related, true) {
		return false
	}
	if related {
		return true
	}
	return currentBPSConfig().ServesModel(model) && !bpsModelBlocked(ctx, store, account.ID(), model)
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

	mu sync.Mutex
	// excluded are accounts whose BPS attempt this request must not retry.
	excluded map[int64]bool
	// blocked: the usage policy blocked this request or its conversation, so
	// no account may serve it through BPS.
	blocked    bool
	orgRetries int
	// probe marks a background usage-policy probe: failures are reported in
	// probeClass instead of changing the account state.
	probe            bool
	probeClass       string
	conversationKeys []string
	servable         map[string]map[int64]bool
	// nativeRetry: accounts whose BPS attempt the usage policy refused and
	// whose native route retries the request (true once the retry took the
	// account).
	nativeRetry map[int64]bool
}

// queueNativeRetry asks for the next attempt to retry account natively, once.
func (s *bpsRequest) queueNativeRetry(accountID int64) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, seen := s.nativeRetry[accountID]; seen {
		return false
	}
	if s.nativeRetry == nil {
		s.nativeRetry = map[int64]bool{}
	}
	s.nativeRetry[accountID] = false
	return true
}

// pendingNativeRetry is the account waiting for its native retry, or 0.
func (s *bpsRequest) pendingNativeRetry() int64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, taken := range s.nativeRetry {
		if !taken {
			return id
		}
	}
	return 0
}

func (s *bpsRequest) takeNativeRetry(accountID int64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if taken, ok := s.nativeRetry[accountID]; ok && !taken {
		s.nativeRetry[accountID] = true
	}
}

// takeOrgRetry spends one of the request's organization rate-limit retries.
func (s *bpsRequest) takeOrgRetry() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.orgRetries >= bpsOrgRetryLimit {
		return false
	}
	s.orgRetries++
	return true
}

func (s *bpsRequest) orgRetryCount() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.orgRetries
}

// bpsStateCache is the runtime cache behind the plugin's account state.
func (s *bpsRequest) cache() cache.TokenCache {
	if s == nil {
		return nil
	}
	return s.handler.bpsCache()
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
	state.conversationKeys = bpsConversationKeys(req, state.inferred)
	state.blocked = currentBPSConfig().PolicyConversationMark && state.conversationBlocked(c.Request.Context())
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

// Admissible admits an account BPS can serve the request on (unless an
// upload cooldown applies to the request's attachments). Otherwise it admits
// the account only for its explicitly enabled native route, or as the last
// resort when no other account could serve (the attempt then fails fast with
// a clear error); a BPS account never spills onto native by default.
func (bpsPlugin) Admissible(ctx context.Context, account *auth.Account, model string) (bool, string) {
	req := plugins.RequestFromContext(ctx)
	state := bpsRequestState(req)
	if state == nil {
		return true, ""
	}
	reason := state.blockReason(ctx, account, model)
	if reason == "" {
		if bpsUploadCooldownForAccount(context.WithValue(ctx, bpsUploadRequestKey{}, state.upload), account) {
			return false, bpsUploadCooldownReason
		}
		return true, ""
	}
	if !state.pinned && bpsNativeExplicit(account, model, state.related) && !nativeRouteOpen(ctx, state.cache(), account) {
		return true, ""
	}
	if reason != "bps_account_refused" && !state.servableElsewhere(ctx, account, model) {
		return true, ""
	}
	return false, reason
}

// Select: BPS serves the attempt when it can, unless the account's native
// route is explicitly on and healthy (native keeps precedence there, as in
// fj-server, unless dual_route_preference=bps) and the conversation is not
// pinned to BPS. An account BPS cannot serve goes
// native only through that explicit route; otherwise BPS keeps the attempt
// and Execute returns the refusal.
func (bpsPlugin) Select(ctx context.Context, attempt plugins.Attempt) bool {
	state := bpsRequestState(attempt.Request)
	related := state != nil && state.related
	if mode, _ := ctx.Value(codexTestModeKey{}).(string); mode == "bps" || mode == "codex" {
		return mode == "bps"
	}
	pinned := state != nil && state.pinned
	native := !pinned && bpsNativeExplicit(attempt.Account, attempt.Model, related) && !nativeRouteOpen(ctx, state.cache(), attempt.Account)
	if reason := state.blockReason(ctx, attempt.Account, attempt.Model); reason != "" {
		if native {
			// BPS is broken for this account: its native route serves.
			attempt.Request.SetUsageMeta(database.TransportNative, bpsRouteReasonMeta(reason))
		}
		return !native
	}
	// Both routes healthy: dual_route_preference decides (native by default).
	return pinned || !native || currentBPSConfig().PrefersBPS()
}

// bpsRouteReasonMeta is the plugin_meta of an attempt a breaker rerouted.
func bpsRouteReasonMeta(reason string) string {
	raw, _ := json.Marshal(map[string]string{"route_reason": reason})
	return string(raw)
}

// PreferredAccounts: for a BPS-only model (bps_only_models) scheduling tries
// accounts BPS can serve it on first instead of native accounts that would
// reject it.
func (bpsPlugin) PreferredAccounts(ctx context.Context, req *plugins.Request, model string) func(*auth.Account) bool {
	if !currentBPSConfig().BPSOnlyModel(model) {
		return nil
	}
	state := bpsRequestState(req)
	p, _ := plugins.Default().Get(BPSPluginID)
	return func(account *auth.Account) bool {
		return p != nil && plugins.Default().EnabledFor(p, account) && state.blockReason(ctx, account, model) == ""
	}
}

func (bpsPlugin) PreferredAccount(ctx context.Context, req *plugins.Request, model string) int64 {
	state := bpsRequestState(req)
	if id := state.pendingNativeRetry(); id > 0 {
		return id
	}
	if state == nil || state.handler == nil {
		return 0
	}
	return state.handler.bpsPreferredTaskAccount(ctx, state.inferred, model)
}

// RetryAccount: after the usage policy refused an account whose native route
// is explicitly on (and healthy), the request retries that account natively
// before any other account.
func (bpsPlugin) RetryAccount(_ context.Context, req *plugins.Request) int64 {
	return bpsRequestState(req).pendingNativeRetry()
}

func (bpsPlugin) AccountSelected(ctx context.Context, req *plugins.Request, account *auth.Account, model string) {
	state := bpsRequestState(req)
	if account != nil {
		state.takeNativeRetry(account.ID())
	}
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
	if mode, _ := ctx.Value(codexTestModeKey{}).(string); state != nil && mode == "" {
		// A full account waits for a slot below instead of being refused.
		if reason := state.blockReason(ctx, env.Account, env.Model); reason != "" && reason != BPSConcurrencyFullReason {
			record, _ := bpsAccountCooling(ctx, state.cache(), env.Account.ID())
			return nil, bpsRefusal(reason, env.Model, record.Until)
		}
	}
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
	if state != nil && bpsNativeExplicit(env.Account, env.Model, state.related) && nativeRouteOpen(ctx, state.cache(), env.Account) {
		bpsSetUsageMeta(env, "route_reason", "native_breaker_open")
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
	// In-flight requests are always tracked (the activity panel); the cap
	// only applies when bps_account_max_concurrency is set.
	release := func() {}
	if env.Account != nil {
		slot, ok := bpsInflightRequests.acquire(ctx, env.Account.ID(), currentBPSConfig().AccountMaxConcurrency, bpsConcurrencyWait)
		if !ok {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, bpsRefusal(BPSConcurrencyFullReason, env.Model, time.Time{})
		}
		release = slot
		bpsNoteRequest(env.Account.ID(), time.Now())
	}
	var resp *http.Response
	var err error
	for {
		resp, err = executeCodexBPS(ctx, pluginServices(env), env.Account, env.Body, env.CacheKey, env.ProxyURL, env.APIKey, deviceCfg, env.Header, env.Compact)
		if err != nil || resp == nil || env.Compact {
			break
		}
		hint, org := bpsPeekOrgRateLimit(resp)
		if !org || !state.takeOrgRetry() {
			break
		}
		// An organization-wide limit: wait it out on the same account; it is
		// neither an account failure nor a strike.
		wait := bpsOrgJitter(bpsOrgPauses.observe(env.Model, hint, time.Now()))
		bpsSetUsageMeta(env, "rate_limit_scope", "org")
		bpsSetUsageMeta(env, "rate_limit_hint_ms", strconv.FormatInt(hint.Milliseconds(), 10))
		bpsSetUsageMeta(env, "org_retries", strconv.Itoa(state.orgRetryCount()))
		resp.Body.Close()
		if err = bpsSleep(ctx, wait); err != nil {
			return nil, err
		}
	}
	if env.Account != nil && (err == nil || resp != nil) {
		window := currentBPSConfig().BudgetWindow()
		bpsAttempts.record(ctx, state.cache(), env.Account.ID(), window)
		if err == nil && resp != nil && resp.StatusCode < 300 {
			bpsBudgets.record(ctx, state.cache(), env.Account.ID(), window)
		}
	}
	if err != nil || resp == nil || resp.Body == nil {
		release()
	} else {
		// The slot is held until the response body is fully read or closed.
		resp.Body = &bpsReleasingBody{ReadCloser: resp.Body, release: release}
	}
	if err == nil && resp != nil && resp.StatusCode == http.StatusOK && !env.Compact && bpsStreamIsEventStream(resp.Header.Get("Content-Type")) {
		request := env.Request
		resp.Body = newBPSStreamGuard(resp.Body, func() { request.SetUsageErrorKind(BPSPluginID, BPSCutoffCompletedKind) })
	}
	if d := bpsAttemptDiagnostic(env); d != nil {
		bpsSetUsageMeta(env, "profile", string(d.Profile))
		if d.AgentIteration != "" {
			bpsSetUsageMeta(env, "agent_iteration", d.AgentIteration)
		}
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

const (
	bpsAttemptHeadersKey = "bps_response_headers"
	bpsAttemptMetaKey    = "bps_usage_meta"
)

// bpsSetUsageMeta sets one key of the attempt's usage_logs.plugin_meta.
func bpsSetUsageMeta(env *plugins.ReqEnv, key, value string) {
	meta, _ := env.State(bpsAttemptMetaKey).(map[string]string)
	if meta == nil {
		meta = map[string]string{}
		env.SetState(bpsAttemptMetaKey, meta)
	}
	meta[key] = value
	encoded, _ := json.Marshal(meta)
	env.Request.SetUsageMeta(BPSPluginID, string(encoded))
}

// bpsRedactedResponseHeaders are the only response headers hidden in the
// compact header record; everything else is kept for analysis.
var bpsRedactedResponseHeaders = map[string]bool{"set-cookie": true, "cookie": true, "authorization": true, "proxy-authorization": true, "chatgpt-account-id": true, "openai-organization": true}

// bpsCompactHeaders renders response headers as "Name: value; ..." sorted by
// name, auth and cookie values redacted, capped at 2 KiB.
func bpsCompactHeaders(header http.Header) string {
	names := make([]string, 0, len(header))
	for name := range header {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		value := strings.Join(header[name], ", ")
		if bpsRedactedResponseHeaders[strings.ToLower(name)] {
			value = "[REDACTED]"
		}
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		b.WriteString(name + ": " + value)
	}
	return security.SafeTruncate(b.String(), 2048)
}

func (bpsPlugin) FilterHeaders(env *plugins.ReqEnv, header http.Header) {
	env.SetState(bpsAttemptHeadersKey, header.Clone())
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
		bpsRecordAttemptFailure(env, status, body)
		env.Request.SetUsageErrorMessage(bpsOriginalErrorMessage(bpsErrorBodySource(gjson.ParseBytes(body)), body))
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
	if source, failed := bpsTerminalEventSource(gjson.ParseBytes(data)); failed {
		env.Request.SetUsageErrorMessage(bpsOriginalErrorMessage(source, nil))
		// In-stream failures are classified like HTTP error bodies.
		if status := bpsStreamFailureStatus(source); status != 0 {
			bpsSetUsageMeta(env, "failure_source", "stream")
			bpsRecordAttemptFailure(env, status, []byte(`{"error":`+bpsJSONObject(source)+`}`))
		}
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

// bpsRecordAttemptFailure updates the plugin's account state after a failed
// BPS attempt; an account-level refusal also keeps this request off that
// account.
func bpsRecordAttemptFailure(env *plugins.ReqEnv, status int, body []byte) {
	if env.Account == nil {
		return
	}
	header, _ := env.State(bpsAttemptHeadersKey).(http.Header)
	bpsSetUsageMeta(env, "error_headers", bpsCompactHeaders(header))
	if state := bpsRequestState(env.Request); state != nil && state.probe {
		state.mu.Lock()
		if state.probeClass == "" {
			state.probeClass = bpsFailureClass(status, body)
			if state.probeClass == "" {
				state.probeClass = "error"
			}
		}
		state.mu.Unlock()
		return
	}
	if hint, ok := bpsOrgRetryable(bpsErrorMessageOf(bpsErrorBodySource(gjson.ParseBytes(body)))); ok {
		env.ClassifyCapture("bps_org_rate_limited")
		// Organization-wide limit (retries exhausted, or output already
		// streamed): not the account's fault, so no cooldown or strike.
		bpsSetUsageMeta(env, "rate_limit_scope", "org")
		bpsSetUsageMeta(env, "rate_limit_hint_ms", strconv.FormatInt(hint.Milliseconds(), 10))
		return
	}
	state := bpsRequestState(env.Request)
	class, record := recordBPSFailure(context.Background(), state.cache(), env.Account.ID(), env.Model, status, header, body)
	if class != "" {
		bpsSetUsageMeta(env, "bps_failure", class)
		env.ClassifyCapture(class)
	}
	if class == BPSRateLimitedReason {
		bpsSetUsageMeta(env, "rate_limit_scope", "account")
	}
	if record.active(time.Now()) && (class == BPSRateLimitedReason || class == BPSPolicyBlockedKind) {
		bpsSetUsageMeta(env, "bps_cooling_until", record.Until.UTC().Format(time.RFC3339))
	}
	if class == BPSPolicyBlockedKind {
		bpsSetUsageMeta(env, "policy_tier", strconv.Itoa(record.PolicyTier))
		bpsSetUsageMeta(env, "policy_strikes", strconv.Itoa(len(record.Strikes)))
		if record.NeedsProbe && len(record.Strikes) == 0 && state != nil && state.handler != nil && state.handler.db != nil {
			// This strike put the account into a cooldown: record the block event.
			if err := state.handler.db.OpenBPSPolicyBlock(context.Background(), env.Account.ID(), record.BlockStarted, record.PolicyTier); err != nil {
				log.Printf("[bps] account=%d record policy block: %v", env.Account.ID(), err)
			}
		}
	}
	if class != "" && class != BPSModelUnavailable {
		// A model refusal only moves that model to native; the account stays.
		bpsExcludeAccountForRequest(env.Request, env.Account)
	}
	if class == BPSPolicyBlockedKind && state != nil && !state.pinned && bpsNativeExplicit(env.Account, env.Model, state.related) &&
		!nativeRouteOpen(context.Background(), state.cache(), env.Account) && state.queueNativeRetry(env.Account.ID()) {
		// Break only BPS: the account's healthy native route retries the
		// request before any other account.
		bpsSetUsageMeta(env, "native_retry", "same_account")
	}
	if class == BPSPolicyBlockedKind {
		env.Request.SetUsageErrorKind(BPSPluginID, BPSPolicyBlockedKind)
		// The block is an account-level request quota: the request fails over
		// to another BPS account (never this one, see the exclusion above).
		// Marking the conversation is opt-in.
		if currentBPSConfig().PolicyConversationMark {
			state.markConversationBlocked(context.Background())
		}
	}
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

type connectionTestUsageKey struct{}

// ConnectionTestUsage carries the transport a connection test was served by,
// so its usage row records it like a normal request's.
type ConnectionTestUsage struct{ req *plugins.Request }

// WithConnectionTestUsage lets ExecuteCodexConnectionTest report its
// transport into the returned ConnectionTestUsage.
func WithConnectionTestUsage(ctx context.Context) (context.Context, *ConnectionTestUsage) {
	usage := &ConnectionTestUsage{}
	return context.WithValue(ctx, connectionTestUsageKey{}, usage), usage
}

// Apply stamps a connection-test usage row with the plugin transport,
// plugin_meta, error details and upstream endpoint when a plugin served the
// test; native tests are left unchanged.
func (u *ConnectionTestUsage) Apply(input *database.UsageLogInput) {
	if u == nil || u.req == nil || input == nil {
		return
	}
	applyTransportPluginUsage(u.req, input)
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
	req.Model = model
	if usage, _ := ctx.Value(connectionTestUsageKey{}).(*ConnectionTestUsage); usage != nil {
		usage.req = req
	}
	registry := plugins.Default()
	var route *plugins.Route
	mode, _ := ctx.Value(codexTestModeKey{}).(string)
	if mode == "codex" {
		if err := ValidateCodexTestMode(ctx, account); err != nil {
			return nil, bpsError(http.StatusBadRequest, "codex_test_mode_unsupported", "%s", err.Error())
		}
	}
	if mode == "bps" {
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
