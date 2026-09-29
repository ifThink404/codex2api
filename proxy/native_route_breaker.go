package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy/plugins"
	"github.com/gin-gonic/gin"
)

// Native route breaker. A dual-route account (BPS enabled and
// codex_native_enabled=true) has two transports; when one goes bad only that
// one is broken, and the account keeps serving through the other:
//
//   - BPS side: the usage-policy cooldown (bps_account_state.go) blocks BPS;
//     the explicit native route then serves the account, and the attempt BPS
//     just refused is retried on the same account natively (RetryAccount).
//   - native side (this file): the upstream silently answering with another
//     model than the one requested ("降智", a degraded bucket) or a native
//     403 opens the account's native route: native_degrade_threshold
//     mismatches (default 2) within native_degrade_window (default 10m), or
//     one 403. The cooldown climbs the native ladder; BPS serves the account
//     meanwhile, and only a background native probe that gets the requested
//     model back closes the breaker.
//
// With both routes broken the account is vetoed (Admissible) and requests go
// to other accounts. Accounts without both routes only have signals logged.

const (
	NativeRouteDegradedReason = "native_degraded"
	NativeTrigger403          = "native_403"
	NativeTriggerModel        = "model_mismatch"

	nativeProbeFallbackModel = "gpt-5.5"
)

func nativeRouteStateKey(accountID int64) string { return "native:" + strconv.FormatInt(accountID, 10) }

// nativeBreakerApplies reports whether account has both routes (BPS enabled
// for it and its native route explicitly on) and the breaker is enabled.
func nativeBreakerApplies(account *auth.Account) bool {
	if account == nil || !currentBPSConfig().NativeBreakerEnabled() || !account.CodexBPSEligible() || !account.CodexNativeRouteExplicit() {
		return false
	}
	p, ok := plugins.Default().Get(BPSPluginID)
	return ok && plugins.Default().EnabledFor(p, account)
}

// open reports whether a route record is broken: cooling, or waiting for the
// probe that closes it.
func (r bpsAccountRecord) open(now time.Time) bool { return r.active(now) || r.NeedsProbe }

// nativeRouteOpen reports whether the account's native route breaker is open.
func nativeRouteOpen(ctx context.Context, store cache.TokenCache, account *auth.Account) bool {
	if account == nil || !currentBPSConfig().NativeBreakerEnabled() {
		return false
	}
	now := time.Now()
	return bpsAccountStateStore.load(ctx, store, nativeRouteStateKey(account.ID()), now).open(now)
}

var modelVariantSuffix = regexp.MustCompile(`-(latest|\d{4}-\d{2}-\d{2}|\d{8})$`)

// nativeModelAliases are model renames the gateway itself applies upstream,
// so the upstream reporting the target is not a downgrade.
var nativeModelAliases = map[string]string{"codex-auto-review": "gpt-5.6-luna"}

func normalizeReportedModel(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if alias, ok := nativeModelAliases[model]; ok {
		model = alias
	}
	return modelVariantSuffix.ReplaceAllString(model, "")
}

// nativeModelMismatch reports whether the upstream-reported model differs
// from every model the gateway may have sent for this row (the client model
// and the effective model after account / global mappings), ignoring the
// gateway's own aliases and variant suffixes (-latest, dates). An empty
// reported model is unknown, never a mismatch.
func nativeModelMismatch(requested, effective, reported string) bool {
	got := normalizeReportedModel(reported)
	if got == "" {
		return false
	}
	for _, sent := range []string{effective, requested} {
		if normalizeReportedModel(sent) == got {
			return false
		}
	}
	return strings.TrimSpace(effective) != "" || strings.TrimSpace(requested) != ""
}

// observeNativeRouteHealth reads a finished usage row: a native attempt's 403
// or upstream model mismatch is a native route signal. Internal rows
// (connection tests) and plugin-served rows are ignored.
func (h *Handler) observeNativeRouteHealth(c *gin.Context, input *database.UsageLogInput) {
	if h == nil || h.store == nil || input == nil || input.AccountID <= 0 || strings.TrimSpace(input.InternalReason) != "" {
		return
	}
	if input.Transport != "" && input.Transport != database.TransportNative {
		return
	}
	var trigger, requested, reported string
	switch {
	case input.StatusCode == http.StatusForbidden && input.UpstreamErrorKind != "cyber_policy":
		trigger, requested = NativeTrigger403, firstNonEmptyString(input.EffectiveModel, input.Model)
	case input.StatusCode < 400 && input.UpstreamModelMismatch != nil && *input.UpstreamModelMismatch &&
		nativeModelMismatch(input.Model, input.EffectiveModel, input.UpstreamResponseModel):
		trigger, requested, reported = NativeTriggerModel, firstNonEmptyString(input.EffectiveModel, input.Model), input.UpstreamResponseModel
	default:
		return
	}
	account := h.store.FindByID(input.AccountID)
	if account == nil || account.IsRelayStyle() {
		return
	}
	var ctx context.Context = context.Background()
	if c != nil && c.Request != nil {
		ctx = context.WithoutCancel(c.Request.Context())
	}
	h.recordNativeRouteSignal(ctx, account, trigger, requested, reported)
}

func nativeSignalDetail(trigger, requested, reported string) string {
	if trigger == NativeTrigger403 {
		return "403 · " + requested
	}
	return requested + " → " + reported
}

// recordNativeRouteSignal counts one native route signal of account and
// opens its native route when the threshold is reached (one 403 suffices).
func (h *Handler) recordNativeRouteSignal(ctx context.Context, account *auth.Account, trigger, requested, reported string) bpsAccountRecord {
	detail := nativeSignalDetail(trigger, requested, reported)
	if !nativeBreakerApplies(account) {
		log.Printf("[native-breaker] account=%d %s (%s) recorded; the account has no BPS route to fall back on", account.ID(), trigger, detail)
		return bpsAccountRecord{}
	}
	cfg := currentBPSConfig()
	ladder := cfg.NativeLadder()
	threshold, window := cfg.NativeThreshold(), cfg.NativeWindow()
	if trigger == NativeTrigger403 {
		threshold = 1
	}
	now := time.Now()
	opened, alreadyOpen := false, false
	record := bpsAccountStateStore.update(ctx, h.bpsCache(), nativeRouteStateKey(account.ID()), now, func(r *bpsAccountRecord) {
		if r.open(now) {
			alreadyOpen = true // a request already in flight; the probe decides
			return
		}
		if !r.LastPolicyBlock.IsZero() {
			clean := now.Sub(r.LastPolicyBlock)
			if clean >= bpsPolicyTierReset {
				r.PolicyTier = 0
			} else {
				r.PolicyTier = max(r.PolicyTier-int(clean/bpsPolicyTierDecay), 0)
			}
		}
		kept := r.Strikes[:0]
		for _, strike := range r.Strikes {
			if now.Sub(strike) < window {
				kept = append(kept, strike)
			}
		}
		r.Strikes = append(kept, now)
		if len(r.Strikes) < threshold {
			return
		}
		r.BlockStarted = r.Strikes[0]
		r.LastPolicyBlock = now
		r.PolicyTier = min(r.PolicyTier+1, len(ladder))
		r.Until, r.Reason = now.Add(ladder[r.PolicyTier-1]), NativeRouteDegradedReason
		r.NeedsProbe, r.NextProbe = true, time.Time{}
		r.Trigger, r.Detail, r.ProbeModel = trigger, detail, requested
		r.Strikes = nil
		opened = true
	})
	if alreadyOpen {
		log.Printf("[native-breaker] account=%d native signal %s (%s) while its native route is already open", account.ID(), trigger, detail)
		return record
	}
	if !opened {
		log.Printf("[native-breaker] account=%d native signal %s (%s): %d/%d in %s", account.ID(), trigger, detail, len(record.Strikes), threshold, window)
		return record
	}
	log.Printf("[native-breaker] account=%d native route OPEN (%s: %s), tier %d/%d; BPS serves the account until %s and a native probe succeeds", account.ID(), trigger, detail, record.PolicyTier, len(ladder), record.Until.Format(time.RFC3339))
	if h.db != nil {
		writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := h.db.OpenRouteBlock(writeCtx, account.ID(), database.BPSRouteNative, record.BlockStarted, record.PolicyTier, trigger+": "+detail); err != nil {
			log.Printf("[native-breaker] account=%d record breaker: %v", account.ID(), err)
		}
		cancel()
	}
	return record
}

// runNativeRouteProbes probes every dual-route account whose native route
// breaker is open and due.
func (h *Handler) runNativeRouteProbes(ctx context.Context, now time.Time) {
	if !currentBPSConfig().NativeBreakerEnabled() {
		return
	}
	store := h.bpsCache()
	for _, account := range h.store.Accounts() {
		if ctx.Err() != nil {
			return
		}
		if !account.CodexBPSEligible() || !account.CodexNativeRouteExplicit() {
			continue
		}
		record := bpsAccountStateStore.load(ctx, store, nativeRouteStateKey(account.ID()), now)
		if record.Reason != NativeRouteDegradedReason || !record.NeedsProbe || record.active(now) || record.NextProbe.After(now) {
			continue
		}
		if release, ok := h.claimRouteProbe(ctx, "native:"+strconv.FormatInt(account.ID(), 10)); ok {
			h.probeNativeRoute(ctx, account, record)
			release()
		}
	}
}

const (
	nativeProbeDegraded = "degraded"
)

// probeNativeRoute sends one native probe and applies its outcome: ok closes
// the breaker, degraded (another model or a 403) climbs the ladder, anything
// else retries a minute later.
func (h *Handler) probeNativeRoute(ctx context.Context, account *auth.Account, open bpsAccountRecord) string {
	model := firstNonEmptyString(open.ProbeModel, h.store.GetTestModel(), nativeProbeFallbackModel)
	result, reported := h.executeNativeProbe(ctx, account, model)
	now := time.Now()
	ladder := currentBPSConfig().NativeLadder()
	record := bpsAccountStateStore.update(ctx, h.bpsCache(), nativeRouteStateKey(account.ID()), now, func(r *bpsAccountRecord) {
		r.LastProbe, r.LastProbeResult = now, result
		switch result {
		case bpsProbeOK:
			r.Until, r.Reason, r.NeedsProbe, r.NextProbe = time.Time{}, "", false, time.Time{}
			r.Trigger, r.Detail, r.Strikes = "", "", nil
		case nativeProbeDegraded:
			r.PolicyTier = min(r.PolicyTier+1, len(ladder))
			r.LastPolicyBlock = now
			r.Until, r.NeedsProbe, r.NextProbe = now.Add(ladder[max(r.PolicyTier, 1)-1]), true, time.Time{}
			if reported != "" {
				r.Detail = model + " → " + reported
			}
		default:
			r.NextProbe = now.Add(bpsProbeRetryDelay)
		}
	})
	if h.db != nil {
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		if err := h.db.RecordRouteProbe(writeCtx, account.ID(), database.BPSRouteNative, now, result, record.PolicyTier); err != nil {
			log.Printf("[native-breaker] account=%d record probe: %v", account.ID(), err)
		}
		if result == bpsProbeOK {
			if duration, found, err := h.db.ClearRouteBlock(writeCtx, account.ID(), database.BPSRouteNative, now); err == nil && found {
				log.Printf("[native-breaker] account=%d native route RECOVERED after %s", account.ID(), duration.Round(time.Second))
			}
		}
		cancel()
	}
	switch result {
	case bpsProbeOK:
		log.Printf("[native-breaker] account=%d native probe got %s back; native route closed", account.ID(), model)
	case nativeProbeDegraded:
		log.Printf("[native-breaker] account=%d native probe still degraded (%s → %s); tier %d/%d, next probe after %s", account.ID(), model, reported, record.PolicyTier, len(ladder), record.Until.Format(time.RFC3339))
	default:
		log.Printf("[native-breaker] account=%d native probe inconclusive (%s); retrying in %s", account.ID(), result, bpsProbeRetryDelay)
	}
	return result
}

// executeNativeProbe sends a tiny native request (not client traffic, not
// logged) and classifies it by the model the upstream reports: ok when it
// is the requested one, degraded for another model or a 403, "error: ..."
// otherwise (including no reported model).
func (h *Handler) executeNativeProbe(ctx context.Context, account *auth.Account, model string) (result, reported string) {
	payload, _ := json.Marshal(map[string]any{
		"model":        model,
		"input":        []map[string]any{{"role": "user", "content": []map[string]string{{"type": "input_text", "text": "ping"}}}},
		"stream":       true,
		"store":        false,
		"instructions": "Reply briefly.",
	})
	probeCtx, cancel := context.WithTimeout(ctx, bpsProbeTimeout)
	defer cancel()
	resp, err := ExecuteRequest(probeCtx, account, payload, NewUpstreamSessionUUID(), account.GetProxyURL(), "", h.deviceCfg, nil, false)
	if err != nil {
		return "error: " + truncateProbeText(err.Error()), ""
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		return nativeProbeDegraded, ""
	}
	if resp.StatusCode >= 300 {
		return fmt.Sprintf("error: HTTP %d", resp.StatusCode), ""
	}
	observer := &upstreamResponseModelObserver{}
	scanner := bufio.NewScanner(io.LimitReader(resp.Body, 4<<20))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if data, ok := strings.CutPrefix(line, "data:"); ok {
			observeUpstreamResponseModelPayload(observer, []byte(strings.TrimSpace(data)), "")
		}
	}
	reported = observer.Model()
	switch {
	case reported == "":
		return "error: no model reported", ""
	case nativeModelMismatch(model, model, reported):
		return nativeProbeDegraded, reported
	}
	return bpsProbeOK, reported
}
