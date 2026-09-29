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
//   - rate limits (BPS 429): a cooldown that honors Retry-After / reset hints
//     and otherwise backs off exponentially from 60s;
//   - usage-policy blocks (403 "blocked by our usage policy"): after
//     policy_block_threshold strikes within 10 minutes, a long cooldown;
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

	bpsRateLimitBase      = 60 * time.Second
	bpsRateLimitMax       = 30 * time.Minute
	bpsRateLimitDecay     = 30 * time.Minute
	bpsPolicyStrikeWindow = 10 * time.Minute
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
}

func (r bpsAccountRecord) active(now time.Time) bool { return r.Until.After(now) }

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
	if remote.Until.After(merged.Until) {
		merged.Until, merged.Reason = remote.Until, remote.Reason
	}
	if remote.LastStrike.After(merged.LastStrike) {
		merged.Level, merged.LastStrike = remote.Level, remote.LastStrike
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
			if !e.record.active(now) && len(e.record.Strikes) == 0 {
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
	s.put(key, record, now)
	if shared := bpsSharedAccountStore(store); shared != nil {
		ttl := time.Until(record.Until)
		if len(record.Strikes) > 0 || !record.LastStrike.IsZero() {
			ttl = max(ttl, bpsRateLimitDecay)
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

// bpsFailureHint is the upstream's own retry hint: Retry-After, or the
// resets_in_seconds / resets_at fields of the error body.
func bpsFailureHint(retryAfter string, body []byte, now time.Time) time.Duration {
	if delay := parseRetryAfterHeaderAt(retryAfter, now); delay > 0 {
		return delay
	}
	for _, path := range []string{"error.resets_in_seconds", "resets_in_seconds", "detail.error.resets_in_seconds"} {
		if secs := gjson.GetBytes(body, path).Int(); secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	for _, path := range []string{"error.resets_at", "resets_at", "detail.error.resets_at"} {
		if at := gjson.GetBytes(body, path).Int(); at > 0 {
			if delay := time.Until(time.Unix(at, 0)); delay > 0 {
				return delay
			}
		}
	}
	return 0
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
	switch {
	case status == http.StatusForbidden && strings.Contains(message, "blocked by our usage policy"):
		return BPSPolicyBlockedKind
	case status == http.StatusTooManyRequests:
		return BPSRateLimitedReason
	}
	return ""
}

// recordBPSFailure updates the account state after a failed BPS attempt and
// returns the failure class ("" for failures the plugin does not track).
func recordBPSFailure(ctx context.Context, store cache.TokenCache, accountID int64, model string, status int, retryAfter string, body []byte) string {
	if accountID <= 0 {
		return ""
	}
	class := bpsFailureClass(status, body)
	now := time.Now()
	switch class {
	case BPSRateLimitedReason:
		hint := bpsFailureHint(retryAfter, body, now)
		record := bpsAccountStateStore.update(ctx, store, bpsAccountStateKey(accountID), now, func(r *bpsAccountRecord) {
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
		record := bpsAccountStateStore.update(ctx, store, bpsAccountStateKey(accountID), now, func(r *bpsAccountRecord) {
			kept := r.Strikes[:0]
			for _, strike := range r.Strikes {
				if now.Sub(strike) < bpsPolicyStrikeWindow {
					kept = append(kept, strike)
				}
			}
			r.Strikes = append(kept, now)
			if len(r.Strikes) >= cfg.PolicyBlockThreshold {
				until := now.Add(time.Duration(cfg.PolicyBlockCooldownHours) * time.Hour)
				if until.After(r.Until) {
					r.Until, r.Reason = until, BPSPolicyBlockedKind
				}
				r.Strikes = nil
			}
		})
		if record.Reason == BPSPolicyBlockedKind && record.active(now) && len(record.Strikes) == 0 {
			log.Printf("[bps] account=%d blocked by the BPS usage policy %d times in %s; BPS disabled for this account until %s", accountID, cfg.PolicyBlockThreshold, bpsPolicyStrikeWindow, record.Until.Format(time.RFC3339))
		} else {
			log.Printf("[bps] account=%d blocked by the BPS usage policy (%d/%d in %s)", accountID, len(record.Strikes), cfg.PolicyBlockThreshold, bpsPolicyStrikeWindow)
		}
	}
	return class
}

// bpsAccountCooling returns the active BPS cooldown of an account, if any.
func bpsAccountCooling(ctx context.Context, store cache.TokenCache, accountID int64) (bpsAccountRecord, bool) {
	now := time.Now()
	record := bpsAccountStateStore.load(ctx, store, bpsAccountStateKey(accountID), now)
	return record, record.active(now)
}

// BPSAccountStatus is the admin view of one account's BPS state.
type BPSAccountStatus struct {
	AccountID     int64     `json:"account_id"`
	CoolingUntil  time.Time `json:"cooling_until,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	PolicyStrikes int       `json:"policy_strikes,omitempty"`
}

// BPSAccountStatuses reports the BPS cooldowns and model blocks of accounts.
func BPSAccountStatuses(ctx context.Context, store cache.TokenCache, accountIDs []int64) []BPSAccountStatus {
	now := time.Now()
	out := make([]BPSAccountStatus, 0, len(accountIDs))
	for _, id := range accountIDs {
		status := BPSAccountStatus{AccountID: id}
		record := bpsAccountStateStore.load(ctx, store, bpsAccountStateKey(id), now)
		if record.active(now) {
			status.CoolingUntil, status.Reason = record.Until, record.Reason
		}
		for _, strike := range record.Strikes {
			if now.Sub(strike) < bpsPolicyStrikeWindow {
				status.PolicyStrikes++
			}
		}
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
