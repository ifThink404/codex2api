package proxy

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/proxy/plugins"
	"github.com/tidwall/gjson"
)

// BPS-scoped account state. With exclude_failures_from_native_health on, BPS
// failures never touch native health or cooldowns, so the plugin keeps its own
// per-account state and enforces it through Select and the Admissible veto:
//
//   - rate limits (BPS 429): an explicit upstream hint (Retry-After,
//     x-ratelimit-reset*, resets_at / resets_in_seconds, retry_after) is
//     honored exactly; otherwise the cooldown backs off exponentially from 60s;
//   - usage-policy blocks (403 "blocked by our usage policy"): every
//     policy_block_threshold blocks within 10 minutes trigger a cooldown, the
//     explicit upstream hint if there is one, else the next tier of
//     bps_policy_cooldown_ladder (a tier is forgiven per 2h without a block,
//     all of them after 24h);
//   - model access (403 basispoints_model_access_changed): "model X is not on
//     BPS for this account" for an hour, so that model is served natively.
//
// State is kept locally and mirrored in the shared runtime cache so every
// replica honors it. Local misses re-read the shared record at most every
// bpsAccountStateRefresh, keeping the scheduler's per-account check cheap.

const (
	bpsAccountStateNamespace = "bps-account-state-v1"
	bpsAccountStateRefresh   = 5 * time.Second
	bpsAccountStateLimit     = 8192

	BPSRateLimitedReason = "bps_rate_limited"
	BPSPolicyBlockedKind = "bps_policy_blocked"
	BPSModelUnavailable  = "bps_model_unavailable"

	bpsModelAccessChanged = "basispoints_model_access_changed"
	bpsModelBlockTTL      = time.Hour

	bpsRateLimitBase      = 60 * time.Second
	bpsRateLimitMax       = 30 * time.Minute
	bpsRateLimitDecay     = 30 * time.Minute
	bpsPolicyStrikeWindow = 10 * time.Minute
	bpsPolicyTierDecay    = 2 * time.Hour
	bpsPolicyTierReset    = 24 * time.Hour
)

// bpsAccountRecord is the shared state of one account or (account, model).
type bpsAccountRecord struct {
	Until  time.Time `json:"until,omitempty"`
	Reason string    `json:"reason,omitempty"`
	// Level is the rate-limit backoff step; LastStrike dates it.
	Level      int       `json:"level,omitempty"`
	LastStrike time.Time `json:"last_strike,omitempty"`
	// Strikes are recent usage-policy blocks inside bpsPolicyStrikeWindow.
	Strikes []time.Time `json:"strikes,omitempty"`
	// PolicyTier is the usage-policy ladder tier reached; LastPolicyBlock
	// dates the latest block, from which tiers are forgiven.
	PolicyTier      int       `json:"policy_tier,omitempty"`
	LastPolicyBlock time.Time `json:"last_policy_block,omitempty"`
	// NeedsProbe: a usage-policy cooldown ends only when a background probe
	// succeeds, never by expiring into real traffic. NextProbe delays a probe
	// retry after a transient probe error.
	NeedsProbe bool `json:"needs_probe,omitempty"`
	// BlockStarted is the first strike of the current block event.
	BlockStarted    time.Time `json:"block_started,omitempty"`
	NextProbe       time.Time `json:"next_probe,omitempty"`
	LastProbe       time.Time `json:"last_probe,omitempty"`
	LastProbeResult string    `json:"last_probe_result,omitempty"`
	// Changed dates the last change of the cooldown fields, so a cleared
	// cooldown replaces an older active one across replicas.
	Changed time.Time `json:"changed,omitempty"`
}

func (r bpsAccountRecord) active(now time.Time) bool { return r.Until.After(now) }

// blocksBPS reports whether the account must not serve BPS: an active
// cooldown, or a usage-policy cooldown still waiting for its probe.
func (r bpsAccountRecord) blocksBPS(now time.Time) bool {
	return r.active(now) || r.Reason == BPSPolicyBlockedKind && r.NeedsProbe
}

type bpsAccountEntry struct {
	record  bpsAccountRecord
	fetched time.Time
}

type bpsAccountStates struct {
	mu      sync.Mutex
	entries map[string]*bpsAccountEntry
}

var bpsAccountStateStore = &bpsAccountStates{}

func bpsAccountStateKey(accountID int64) string { return "account:" + strconv.FormatInt(accountID, 10) }

func bpsModelStateKey(accountID int64, model string) string {
	return "model:" + strconv.FormatInt(accountID, 10) + ":" + strings.ToLower(strings.TrimSpace(model))
}

func bpsSharedAccountStore(store cache.TokenCache) cache.TokenCache {
	store = bpsGuardedCache(store)
	if store == nil || !store.SharedAcrossInstances() {
		return nil
	}
	return store
}

// load returns the record for key, refreshing it from the shared cache when
// the local copy is missing or older than bpsAccountStateRefresh.
func (s *bpsAccountStates) load(ctx context.Context, store cache.TokenCache, key string, now time.Time) bpsAccountRecord {
	s.mu.Lock()
	entry := s.entries[key]
	if entry != nil && (entry.record.active(now) || now.Sub(entry.fetched) < bpsAccountStateRefresh) {
		record := entry.record
		s.mu.Unlock()
		return record
	}
	s.mu.Unlock()
	var record bpsAccountRecord
	if entry != nil {
		record = entry.record
	}
	if shared := bpsSharedAccountStore(store); shared != nil {
		readCtx, cancel := context.WithTimeout(ctx, bpsAttachmentCacheTimeout)
		raw, found, err := shared.GetRuntime(readCtx, bpsAccountStateNamespace, key)
		cancel()
		if err == nil && found && len(raw) <= 4096 {
			var remote bpsAccountRecord
			if json.Unmarshal(raw, &remote) == nil {
				record = mergeBPSAccountRecords(record, remote)
			}
		}
	}
	s.put(key, record, now)
	return record
}

// mergeBPSAccountRecords keeps the later cooldown and the union of strikes.
func mergeBPSAccountRecords(local, remote bpsAccountRecord) bpsAccountRecord {
	merged := local
	switch {
	case remote.Changed.After(local.Changed):
		merged.Until, merged.Reason, merged.Changed = remote.Until, remote.Reason, remote.Changed
		merged.NeedsProbe, merged.NextProbe = remote.NeedsProbe, remote.NextProbe
		merged.LastProbe, merged.LastProbeResult = remote.LastProbe, remote.LastProbeResult
	case remote.Changed.Equal(local.Changed) && remote.Until.After(merged.Until):
		merged.Until, merged.Reason = remote.Until, remote.Reason
	}
	if remote.LastStrike.After(merged.LastStrike) {
		merged.Level, merged.LastStrike = remote.Level, remote.LastStrike
	}
	if remote.LastPolicyBlock.After(merged.LastPolicyBlock) {
		merged.PolicyTier, merged.LastPolicyBlock = remote.PolicyTier, remote.LastPolicyBlock
	}
	seen := make(map[int64]bool, len(merged.Strikes))
	for _, strike := range merged.Strikes {
		seen[strike.UnixNano()] = true
	}
	for _, strike := range remote.Strikes {
		if !seen[strike.UnixNano()] {
			merged.Strikes = append(merged.Strikes, strike)
		}
	}
	return merged
}

func (s *bpsAccountStates) put(key string, record bpsAccountRecord, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = make(map[string]*bpsAccountEntry)
	}
	if _, exists := s.entries[key]; !exists && len(s.entries) >= bpsAccountStateLimit {
		for k, e := range s.entries {
			if !e.record.blocksBPS(now) && len(e.record.Strikes) == 0 {
				delete(s.entries, k)
			}
		}
		if len(s.entries) >= bpsAccountStateLimit {
			for k := range s.entries {
				delete(s.entries, k)
				break
			}
		}
	}
	s.entries[key] = &bpsAccountEntry{record: record, fetched: now}
}

// update applies change to the freshest record for key and publishes it.
func (s *bpsAccountStates) update(ctx context.Context, store cache.TokenCache, key string, now time.Time, change func(*bpsAccountRecord)) bpsAccountRecord {
	s.mu.Lock()
	if entry := s.entries[key]; entry != nil {
		entry.fetched = time.Time{}
	}
	s.mu.Unlock()
	record := s.load(ctx, store, key, now)
	change(&record)
	record.Changed = now
	s.put(key, record, now)
	if shared := bpsSharedAccountStore(store); shared != nil {
		ttl := time.Until(record.Until)
		if len(record.Strikes) > 0 || !record.LastStrike.IsZero() {
			ttl = max(ttl, bpsRateLimitDecay)
		}
		if record.PolicyTier > 0 {
			ttl = max(ttl, bpsPolicyTierReset)
		}
		if record.NeedsProbe {
			ttl = max(ttl, 7*24*time.Hour)
		}
		if ttl > 0 {
			raw, _ := json.Marshal(record)
			writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bpsAttachmentCacheTimeout)
			_ = shared.SetRuntime(writeCtx, bpsAccountStateNamespace, key, raw, ttl)
			cancel()
		}
	}
	return record
}

// bpsFailureHint is the upstream's own cooldown hint, from headers
// (Retry-After, Retry-After-Ms, x-ratelimit-reset*) or the error body
// (resets_in_seconds, resets_at, retry_after[_seconds]). The longest hint
// wins; 0 means the upstream gave none.
func bpsFailureHint(header http.Header, body []byte, now time.Time) time.Duration {
	var hint time.Duration
	take := func(d time.Duration) {
		if d > 0 && d <= 7*24*time.Hour && d > hint {
			hint = d
		}
	}
	take(parseRetryAfterHeaderAt(header.Get("Retry-After"), now))
	if ms, err := strconv.ParseInt(strings.TrimSpace(header.Get("Retry-After-Ms")), 10, 64); err == nil {
		take(time.Duration(ms) * time.Millisecond)
	}
	for name, values := range header {
		if !strings.HasPrefix(strings.ToLower(name), "x-ratelimit-reset") || len(values) == 0 {
			continue
		}
		take(bpsResetValue(values[0], now))
	}
	root := gjson.ParseBytes(body)
	for _, source := range []gjson.Result{root, root.Get("error"), root.Get("detail"), root.Get("detail.error"), root.Get("detail.error.error")} {
		for _, key := range []string{"resets_in_seconds", "retry_after", "retry_after_seconds"} {
			if secs := source.Get(key); secs.Type == gjson.Number && secs.Float() > 0 {
				take(time.Duration(secs.Float() * float64(time.Second)))
			}
		}
		if at := source.Get("resets_at").Int(); at > 0 {
			take(time.Unix(at, 0).Sub(now))
		}
	}
	return hint
}

// bpsResetValue reads an x-ratelimit-reset* value: a Go-style duration
// ("6m0s", "1.5s", "20ms"), seconds, or a Unix timestamp.
func bpsResetValue(raw string, now time.Time) time.Duration {
	raw = strings.TrimSpace(raw)
	if d, err := time.ParseDuration(raw); err == nil {
		return d
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || value <= 0 {
		return 0
	}
	if value > 1e9 {
		return time.Unix(int64(value), 0).Sub(now)
	}
	return time.Duration(value * float64(time.Second))
}

// bpsFailureClass classifies a BPS error for the plugin's own account state.
// Usage-policy blocks are matched on the raw provider message (they arrive as
// type server_error with no code).
func bpsFailureClass(status int, body []byte) string {
	root := gjson.ParseBytes(body)
	source := bpsErrorBodySource(root)
	message := strings.ToLower(source.Get("message").String() + " " + root.Get("message").String())
	if source.Type == gjson.String {
		message += " " + strings.ToLower(source.String())
	}
	code := strings.TrimSpace(source.Get("code").String())
	if code == "" {
		code = strings.TrimSpace(root.Get("code").String())
	}
	switch {
	case code == bpsModelAccessChanged || strings.Contains(message, bpsModelAccessChanged):
		return BPSModelUnavailable
	case status == http.StatusForbidden && strings.Contains(message, "blocked by our usage policy"):
		return BPSPolicyBlockedKind
	case status == http.StatusTooManyRequests:
		return BPSRateLimitedReason
	}
	return ""
}

// bpsStreamFailureStatus maps the error object of a failed stream event to
// the HTTP status its HTTP counterpart carries (403 usage-policy block or
// model access, 429 rate limit), or 0 for failures the plugin does not track.
func bpsStreamFailureStatus(source gjson.Result) int {
	code := strings.ToLower(strings.TrimSpace(source.Get("code").String()))
	kind := strings.ToLower(strings.TrimSpace(source.Get("type").String()))
	message := strings.ToLower(source.Get("message").String())
	if source.Type == gjson.String {
		message = strings.ToLower(source.String())
	}
	switch {
	case strings.Contains(message, "blocked by our usage policy"), code == bpsModelAccessChanged, strings.Contains(message, bpsModelAccessChanged):
		return http.StatusForbidden
	case code == "rate_limit_exceeded", kind == "rate_limit_error", kind == "usage_limit_reached", strings.Contains(message, "rate limit"):
		return http.StatusTooManyRequests
	}
	return 0
}

// bpsJSONObject is source as a JSON object ({"message": ...} for a string).
func bpsJSONObject(source gjson.Result) string {
	if source.IsObject() {
		return source.Raw
	}
	raw, _ := json.Marshal(map[string]string{"message": source.String()})
	return string(raw)
}

// recordBPSFailure updates the account state after a failed BPS attempt and
// returns the failure class ("" for failures the plugin does not track).
func recordBPSFailure(ctx context.Context, store cache.TokenCache, accountID int64, model string, status int, header http.Header, body []byte) (string, bpsAccountRecord) {
	var record bpsAccountRecord
	if accountID <= 0 {
		return "", record
	}
	class := bpsFailureClass(status, body)
	now := time.Now()
	switch class {
	case BPSRateLimitedReason:
		hint := bpsFailureHint(header, body, now)
		record = bpsAccountStateStore.update(ctx, store, bpsAccountStateKey(accountID), now, func(r *bpsAccountRecord) {
			if now.Sub(r.LastStrike) > bpsRateLimitDecay {
				r.Level = 0
			}
			r.Level = min(r.Level+1, 16)
			r.LastStrike = now
			delay := hint
			if delay <= 0 {
				delay = min(bpsRateLimitBase<<(r.Level-1), bpsRateLimitMax)
			}
			if until := now.Add(delay); until.After(r.Until) {
				r.Until, r.Reason = until, BPSRateLimitedReason
			}
		})
		log.Printf("[bps] account=%d rate limited on BPS; BPS cooling until %s (level %d)", accountID, record.Until.Format(time.RFC3339), record.Level)
	case BPSPolicyBlockedKind:
		cfg := currentBPSConfig()
		ladder := cfg.PolicyLadder()
		hint := bpsFailureHint(header, body, now)
		escalated := false
		record = bpsAccountStateStore.update(ctx, store, bpsAccountStateKey(accountID), now, func(r *bpsAccountRecord) {
			if !r.LastPolicyBlock.IsZero() {
				clean := now.Sub(r.LastPolicyBlock)
				if clean >= bpsPolicyTierReset {
					r.PolicyTier = 0
				} else {
					r.PolicyTier = max(r.PolicyTier-int(clean/bpsPolicyTierDecay), 0)
				}
			}
			r.LastPolicyBlock = now
			kept := r.Strikes[:0]
			for _, strike := range r.Strikes {
				if now.Sub(strike) < bpsPolicyStrikeWindow {
					kept = append(kept, strike)
				}
			}
			r.Strikes = append(kept, now)
			if len(r.Strikes) < cfg.PolicyBlockThreshold {
				return
			}
			if !r.NeedsProbe {
				// A new block event starts at its first strike.
				r.BlockStarted = now
				for _, strike := range r.Strikes {
					if strike.Before(r.BlockStarted) {
						r.BlockStarted = strike
					}
				}
			}
			r.PolicyTier = min(r.PolicyTier+1, len(ladder))
			delay := hint
			if delay <= 0 {
				delay = ladder[r.PolicyTier-1]
			}
			if until := now.Add(delay); until.After(r.Until) || r.Reason != BPSPolicyBlockedKind {
				r.Until, r.Reason = until, BPSPolicyBlockedKind
			}
			r.NeedsProbe, r.NextProbe = true, time.Time{}
			r.Strikes = nil
			escalated = true
		})
		if escalated {
			source := "ladder"
			if hint > 0 {
				source = "upstream hint"
			}
			log.Printf("[bps] account=%d blocked by the BPS usage policy %d times in %s; policy tier %d/%d, BPS cooling until %s (%s)", accountID, cfg.PolicyBlockThreshold, bpsPolicyStrikeWindow, record.PolicyTier, len(ladder), record.Until.Format(time.RFC3339), source)
		} else {
			log.Printf("[bps] account=%d blocked by the BPS usage policy (%d/%d in %s, tier %d/%d)", accountID, len(record.Strikes), cfg.PolicyBlockThreshold, bpsPolicyStrikeWindow, record.PolicyTier, len(ladder))
		}
	case BPSModelUnavailable:
		if strings.TrimSpace(model) == "" {
			return class, record
		}
		bpsAccountStateStore.update(ctx, store, bpsModelStateKey(accountID, model), now, func(r *bpsAccountRecord) {
			r.Until, r.Reason = now.Add(bpsModelBlockTTL), BPSModelUnavailable
		})
		log.Printf("[bps] account=%d has no BPS access to model %s; serving it natively for %s", accountID, model, bpsModelBlockTTL)
	}
	return class, record
}

// bpsAccountCooling returns the active BPS cooldown of an account, if any.
func bpsAccountCooling(ctx context.Context, store cache.TokenCache, accountID int64) (bpsAccountRecord, bool) {
	now := time.Now()
	record := bpsAccountStateStore.load(ctx, store, bpsAccountStateKey(accountID), now)
	return record, record.blocksBPS(now)
}

// bpsModelBlocked reports whether BPS refused model for this account recently.
func bpsModelBlocked(ctx context.Context, store cache.TokenCache, accountID int64, model string) bool {
	if strings.TrimSpace(model) == "" {
		return false
	}
	now := time.Now()
	return bpsAccountStateStore.load(ctx, store, bpsModelStateKey(accountID, model), now).active(now)
}

// BPSAccountStatus is the admin view of one account's BPS state.
type BPSAccountStatus struct {
	AccountID     int64     `json:"account_id"`
	CoolingUntil  time.Time `json:"cooling_until,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	PolicyStrikes int       `json:"policy_strikes,omitempty"`
	// PolicyTier is the usage-policy ladder tier reached, of PolicyTiers.
	PolicyTier  int `json:"policy_tier,omitempty"`
	PolicyTiers int `json:"policy_tiers"`
	// ProbePending: a usage-policy cooldown that ends only after a background
	// probe succeeds (LastProbe / LastProbeResult: ok, blocked, error: ...).
	ProbePending bool `json:"probe_pending,omitempty"`
	// NextProbeAt: when the pending probe runs (the tier's end, or the retry
	// time after an inconclusive probe).
	NextProbeAt     time.Time `json:"next_probe_at,omitempty"`
	LastProbe       time.Time `json:"last_probe,omitempty"`
	LastProbeResult string    `json:"last_probe_result,omitempty"`
	// InFlight BPS requests on this replica, of MaxConcurrency (0 = no cap).
	InFlight       int `json:"in_flight"`
	MaxConcurrency int `json:"max_concurrency"`
	// BudgetUsed successful BPS requests in the window, of Budget (0 = off);
	// Attempts is every BPS upstream attempt in the window. Both are counted
	// across replicas; InFlight and LastRequestAt are this replica's.
	BudgetUsed    int       `json:"budget_used"`
	Budget        int       `json:"budget"`
	Attempts      int       `json:"attempts"`
	LastRequestAt time.Time `json:"last_request_at,omitempty"`
	// ModelsUnavailable maps models BPS refused for this account to the time
	// they are retried on BPS.
	ModelsUnavailable map[string]time.Time `json:"models_unavailable,omitempty"`
}

// BPSAccountStatuses reports the BPS cooldowns and model blocks of accounts.
func BPSAccountStatuses(ctx context.Context, store cache.TokenCache, accountIDs []int64) []BPSAccountStatus {
	now := time.Now()
	out := make([]BPSAccountStatus, 0, len(accountIDs))
	for _, id := range accountIDs {
		status := BPSAccountStatus{AccountID: id}
		status.BudgetUsed, status.Budget, status.InFlight, status.MaxConcurrency = bpsBudgetUsage(ctx, store, id)
		status.Attempts = bpsAttempts.used(ctx, store, id, currentBPSConfig().BudgetWindow())
		status.LastRequestAt = bpsLastRequestAt(id)
		record := bpsAccountStateStore.load(ctx, store, bpsAccountStateKey(id), now)
		if record.active(now) {
			status.CoolingUntil, status.Reason = record.Until, record.Reason
		}
		status.ProbePending = record.Reason == BPSPolicyBlockedKind && record.NeedsProbe
		if status.ProbePending {
			status.NextProbeAt = record.Until
			if record.NextProbe.After(status.NextProbeAt) {
				status.NextProbeAt = record.NextProbe
			}
		}
		status.LastProbe, status.LastProbeResult = record.LastProbe, record.LastProbeResult
		for _, strike := range record.Strikes {
			if now.Sub(strike) < bpsPolicyStrikeWindow {
				status.PolicyStrikes++
			}
		}
		status.PolicyTiers = len(currentBPSConfig().PolicyLadder())
		if !record.LastPolicyBlock.IsZero() {
			clean := now.Sub(record.LastPolicyBlock)
			if clean < bpsPolicyTierReset {
				status.PolicyTier = max(record.PolicyTier-int(clean/bpsPolicyTierDecay), 0)
			}
		}
		prefix := "model:" + strconv.FormatInt(id, 10) + ":"
		bpsAccountStateStore.mu.Lock()
		for key, entry := range bpsAccountStateStore.entries {
			if strings.HasPrefix(key, prefix) && entry.record.active(now) {
				if status.ModelsUnavailable == nil {
					status.ModelsUnavailable = map[string]time.Time{}
				}
				status.ModelsUnavailable[strings.TrimPrefix(key, prefix)] = entry.record.Until
			}
		}
		bpsAccountStateStore.mu.Unlock()
		out = append(out, status)
	}
	return out
}

// bpsExcludeAccountForRequest keeps a request from retrying an account whose
// BPS attempt was refused for an account-level reason.
func bpsExcludeAccountForRequest(req *plugins.Request, account *auth.Account) {
	state := bpsRequestState(req)
	if state == nil || account == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.excluded == nil {
		state.excluded = map[int64]bool{}
	}
	state.excluded[account.ID()] = true
}

func (s *bpsRequest) accountExcluded(account *auth.Account) bool {
	if s == nil || account == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.excluded[account.ID()]
}

// BPSDashboardSettings are the plugin settings the admin dashboard shows:
// the usable-account warning floor (default 2) and the budget window.
func BPSDashboardSettings() (minUsable int, budgetWindow time.Duration) {
	cfg := currentBPSConfig()
	minUsable = cfg.MinUsableAccounts
	if minUsable <= 0 {
		minUsable = 2
	}
	return minUsable, cfg.BudgetWindow()
}
