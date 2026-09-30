package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy/degradejudge"
	"github.com/codex2api/proxy/plugins"
)

// Per-route degradation ("降智") breaker. An account's routes are native
// and BPS; when one goes bad only that route breaks and the account keeps
// serving through the other (a dual-route account: BPS enabled and
// codex_native_enabled=true). The judge is the operators' pelican probe
// (degradejudge): a tiny internal request drawing a pelican on a bicycle in
// SVG whose complexity score tells a coherent model from a degraded bucket.
//
//   - An upstream model mismatch on a client request (the reported model is
//     not the requested one, the gateway's own aliases and variants aside)
//     only schedules a pelican CONFIRM probe of that account and route.
//   - A pelican verdict below degrade_score_threshold, or a native 403,
//     breaks the route. The cooldown climbs degrade_cooldown_ladder; when it
//     ends a RECOVERY pelican probe decides: ok (and the reported model
//     matches) closes the route, degraded climbs a tier, an invalid sample
//     retries a minute later.
//   - A broken native route sends the account's traffic to BPS; a broken BPS
//     route vetoes BPS for it (native serves when explicitly on); with both
//     broken the account is skipped.
//   - Optional periodic sampling (degrade_probe_interval) probes every
//     BPS-enabled account's BPS route, and its native route when explicitly
//     enabled. At most degrade_probe_max_concurrent probes run per replica.
//
// The client path only enqueues signals (observeRouteHealth); everything
// else runs on background goroutines.

const (
	RouteDegradedReason = "route_degraded"
	BPSDegradedReason   = "bps_degraded"

	RouteNative = database.BPSRouteNative
	RouteBPS    = database.BPSRouteBPS

	DegradeTriggerManual    = "manual"
	DegradeTriggerMismatch  = "mismatch"
	DegradeTriggerScheduled = "scheduled"
	DegradeTriggerRecovery  = "recovery"

	degradeBreakTriggerPelican = "pelican"
	degradeBreakTrigger403     = "native_403"

	degradeProbeTimeout  = 10 * time.Minute
	degradeLeaseTTL      = degradeProbeTimeout + time.Minute
	degradeLeaseNS       = "degrade-probe-v1"
	degradeSignalBuffer  = 256
	degradeInvalidRetry  = time.Minute
	degradeSchedulerTick = 20 * time.Second
)

// routeBreakerKey is the account-state key of a route's degradation breaker.
func routeBreakerKey(route string, accountID int64) string {
	if route == RouteNative {
		return "native:" + strconv.FormatInt(accountID, 10)
	}
	return "bpsdeg:" + strconv.FormatInt(accountID, 10)
}

func bpsServedAccount(account *auth.Account) bool {
	if account == nil || !account.CodexBPSEligible() {
		return false
	}
	p, ok := plugins.Default().Get(BPSPluginID)
	return ok && plugins.Default().EnabledFor(p, account)
}

// degradeBreakerApplies reports whether route of account can be broken: the
// BPS route of any BPS-served account, the native route of a dual-route one.
func degradeBreakerApplies(account *auth.Account, route string) bool {
	if account == nil || !currentBPSConfig().DegradeEnabled() || !bpsServedAccount(account) {
		return false
	}
	return route == RouteBPS || route == RouteNative && account.CodexNativeRouteExplicit()
}

// open reports whether a route record is broken: cooling, or waiting for the
// probe that closes it.
func (r bpsAccountRecord) open(now time.Time) bool { return r.active(now) || r.NeedsProbe }

// routeBreakerOpen reports whether route of account is broken.
func routeBreakerOpen(ctx context.Context, store cache.TokenCache, account *auth.Account, route string) bool {
	if account == nil || !currentBPSConfig().DegradeEnabled() {
		return false
	}
	now := time.Now()
	record := bpsAccountStateStore.load(ctx, store, routeBreakerKey(route, account.ID()), now)
	return record.Reason == RouteDegradedReason && record.open(now)
}

// nativeRouteOpen reports whether the account's native route is broken.
func nativeRouteOpen(ctx context.Context, store cache.TokenCache, account *auth.Account) bool {
	return routeBreakerOpen(ctx, store, account, RouteNative)
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
// from every model the gateway may have sent (the client model and the
// effective model after account / global mappings), ignoring the gateway's
// own aliases and variant suffixes (-latest, dates). An empty reported model
// is unknown, never a mismatch.
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

// routeSignal is one client-path observation handed to the background.
type routeSignal struct {
	accountID           int64
	route               string
	forbidden           bool // a native 403; otherwise a model mismatch
	requested, reported string
}

type degradeJob struct {
	accountID int64
	route     string
	trigger   string
}

func (j degradeJob) key() string { return j.route + ":" + strconv.FormatInt(j.accountID, 10) }

// degradeRuntime queues signals and pelican probes of one handler.
type degradeRuntime struct {
	signals chan routeSignal

	mu      sync.Mutex
	queue   []degradeJob
	pending map[string]string // key -> queued / running
	running int
	wake    chan struct{}
	sampled map[string]time.Time // next scheduled probe per route
}

func (h *Handler) degradeRuntime() *degradeRuntime {
	h.degradeOnce.Do(func() {
		h.degrade = &degradeRuntime{
			signals: make(chan routeSignal, degradeSignalBuffer),
			pending: map[string]string{},
			wake:    make(chan struct{}, 1),
			sampled: map[string]time.Time{},
		}
	})
	return h.degrade
}

// observeRouteHealth is the client-path hook of a finished usage row: a
// strict no-op unless the breaker is on and the row is a non-internal native
// 403 or an upstream model mismatch of a route the breaker applies to. It
// never modifies the row and does no I/O: the signal goes to a bounded
// channel and is dropped when the channel is full.
func (h *Handler) observeRouteHealth(input *database.UsageLogInput) {
	if h == nil || h.store == nil || input == nil || input.AccountID <= 0 || strings.TrimSpace(input.InternalReason) != "" || !currentBPSConfig().DegradeEnabled() {
		return
	}
	route := RouteNative
	switch input.Transport {
	case "", database.TransportNative:
	case BPSPluginID:
		route = RouteBPS
	default:
		return
	}
	signal := routeSignal{accountID: input.AccountID, route: route, requested: firstNonEmptyString(input.EffectiveModel, input.Model)}
	switch {
	case route == RouteNative && input.StatusCode == http.StatusForbidden && input.UpstreamErrorKind != "cyber_policy":
		signal.forbidden = true
	case input.StatusCode < 400 && input.UpstreamModelMismatch != nil && *input.UpstreamModelMismatch &&
		nativeModelMismatch(input.Model, input.EffectiveModel, input.UpstreamResponseModel):
		signal.reported = input.UpstreamResponseModel
	default:
		return
	}
	if !degradeBreakerApplies(h.store.FindByID(input.AccountID), route) {
		return
	}
	select {
	case h.degradeRuntime().signals <- signal:
	default: // full: drop, the next signal will do
	}
}

// handleRouteSignal applies one signal (background): a native 403 breaks the
// native route at once; a model mismatch schedules a confirm probe.
func (h *Handler) handleRouteSignal(ctx context.Context, signal routeSignal) {
	account := h.store.FindByID(signal.accountID)
	if !degradeBreakerApplies(account, signal.route) {
		return
	}
	if signal.forbidden {
		h.breakRoute(ctx, account, signal.route, degradeBreakTrigger403, "403 · "+signal.requested, signal.requested)
		return
	}
	if routeBreakerOpen(ctx, h.bpsCache(), account, signal.route) {
		return // the recovery probe decides
	}
	if h.EnqueueDegradeProbe(account.ID(), signal.route, DegradeTriggerMismatch) {
		log.Printf("[degrade] account=%d %s route reported %s for %s; pelican confirm probe queued", account.ID(), signal.route, signal.reported, signal.requested)
	}
}

// breakRoute opens route of account (or climbs a tier when already open).
func (h *Handler) breakRoute(ctx context.Context, account *auth.Account, route, trigger, detail, probeModel string) bpsAccountRecord {
	ladder := currentBPSConfig().DegradeLadder()
	now := time.Now()
	opened := false
	record := bpsAccountStateStore.update(ctx, h.bpsCache(), routeBreakerKey(route, account.ID()), now, func(r *bpsAccountRecord) {
		wasOpen := r.Reason == RouteDegradedReason && r.open(now)
		if !wasOpen && !r.LastPolicyBlock.IsZero() {
			clean := now.Sub(r.LastPolicyBlock)
			if clean >= bpsPolicyTierReset {
				r.PolicyTier = 0
			} else {
				r.PolicyTier = max(r.PolicyTier-int(clean/bpsPolicyTierDecay), 0)
			}
		}
		if !wasOpen {
			r.BlockStarted = now
			opened = true
		}
		r.LastPolicyBlock = now
		r.PolicyTier = min(r.PolicyTier+1, len(ladder))
		r.Until, r.Reason = now.Add(ladder[r.PolicyTier-1]), RouteDegradedReason
		r.NeedsProbe, r.NextProbe = true, time.Time{}
		r.Trigger, r.Detail = trigger, detail
		if probeModel != "" {
			r.ProbeModel = probeModel
		}
	})
	if opened {
		log.Printf("[degrade] account=%d %s route BROKEN (%s: %s), tier %d/%d until %s; a pelican probe decides recovery", account.ID(), route, trigger, detail, record.PolicyTier, len(ladder), record.Until.Format(time.RFC3339))
	} else {
		log.Printf("[degrade] account=%d %s route still degraded (%s); tier %d/%d until %s", account.ID(), route, detail, record.PolicyTier, len(ladder), record.Until.Format(time.RFC3339))
	}
	if h.db != nil {
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		if err := h.db.OpenRouteBlock(writeCtx, account.ID(), route, database.RouteBlockDegrade, record.BlockStarted, record.PolicyTier, trigger+": "+detail); err != nil {
			log.Printf("[degrade] account=%d record breaker: %v", account.ID(), err)
		}
		cancel()
	}
	return record
}

// closeRoute closes an open route breaker after a passing probe.
func (h *Handler) closeRoute(ctx context.Context, account *auth.Account, route string) {
	now := time.Now()
	bpsAccountStateStore.update(ctx, h.bpsCache(), routeBreakerKey(route, account.ID()), now, func(r *bpsAccountRecord) {
		r.Until, r.Reason, r.NeedsProbe, r.NextProbe = time.Time{}, "", false, time.Time{}
		r.Trigger, r.Detail = "", ""
	})
	if h.db != nil {
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		if duration, found, err := h.db.ClearRouteBlock(writeCtx, account.ID(), route, database.RouteBlockDegrade, now); err == nil && found {
			log.Printf("[degrade] account=%d %s route RECOVERED after %s", account.ID(), route, duration.Round(time.Second))
		}
		cancel()
	}
}

// EnqueueDegradeProbe queues a pelican probe of route on the account unless
// one is already queued or running for it; false when deduplicated.
func (h *Handler) EnqueueDegradeProbe(accountID int64, route, trigger string) bool {
	rt := h.degradeRuntime()
	job := degradeJob{accountID: accountID, route: route, trigger: trigger}
	rt.mu.Lock()
	if _, busy := rt.pending[job.key()]; busy {
		rt.mu.Unlock()
		return false
	}
	rt.pending[job.key()] = "queued"
	rt.queue = append(rt.queue, job)
	rt.mu.Unlock()
	select {
	case rt.wake <- struct{}{}:
	default:
	}
	return true
}

// DegradeProbePending is "queued", "running" or "" for route of an account.
func (h *Handler) DegradeProbePending(accountID int64, route string) string {
	rt := h.degradeRuntime()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.pending[degradeJob{accountID: accountID, route: route}.key()]
}

// takeDegradeJob starts the next queued job when a slot is free.
func (rt *degradeRuntime) takeDegradeJob(limit int) (degradeJob, bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.queue) == 0 || rt.running >= limit {
		return degradeJob{}, false
	}
	job := rt.queue[0]
	rt.queue = rt.queue[1:]
	rt.running++
	rt.pending[job.key()] = "running"
	return job, true
}

func (rt *degradeRuntime) finishDegradeJob(job degradeJob) {
	rt.mu.Lock()
	rt.running--
	delete(rt.pending, job.key())
	rt.mu.Unlock()
	select {
	case rt.wake <- struct{}{}:
	default:
	}
}

// startDegradeRuntime runs the signal consumer, the probe dispatcher and the
// scheduler (recovery and periodic sampling) until ctx ends.
func (h *Handler) startDegradeRuntime(ctx context.Context) {
	rt := h.degradeRuntime()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case signal := <-rt.signals:
				h.handleRouteSignal(ctx, signal)
			}
		}
	}()
	go func() {
		ticker := time.NewTicker(degradeSchedulerTick)
		defer ticker.Stop()
		for {
			for {
				job, ok := rt.takeDegradeJob(currentBPSConfig().DegradeMaxConcurrent())
				if !ok {
					break
				}
				go func() {
					defer rt.finishDegradeJob(job)
					h.runDegradeJob(ctx, job)
				}()
			}
			select {
			case <-ctx.Done():
				return
			case <-rt.wake:
			case now := <-ticker.C:
				h.scheduleDegradeProbes(ctx, now)
			}
		}
	}()
}

// scheduleDegradeProbes queues recovery probes of broken routes whose
// cooldown ended and, when sampling is on, the periodic probes that are due
// (staggered over the interval; cooling, disabled or invalid accounts skip).
func (h *Handler) scheduleDegradeProbes(ctx context.Context, now time.Time) {
	cfg := currentBPSConfig()
	if !cfg.DegradeEnabled() {
		return
	}
	rt := h.degradeRuntime()
	interval := cfg.DegradeInterval()
	store := h.bpsCache()
	for _, account := range h.store.Accounts() {
		if ctx.Err() != nil {
			return
		}
		for _, route := range []string{RouteBPS, RouteNative} {
			if !degradeBreakerApplies(account, route) {
				continue
			}
			record := bpsAccountStateStore.load(ctx, store, routeBreakerKey(route, account.ID()), now)
			if record.Reason == RouteDegradedReason && record.NeedsProbe {
				if !record.active(now) && !record.NextProbe.After(now) {
					h.EnqueueDegradeProbe(account.ID(), route, DegradeTriggerRecovery)
				}
				continue
			}
			if interval <= 0 || !account.IsEnabled() || account.CredentialInvalid() {
				continue
			}
			if route == RouteBPS {
				if _, cooling := bpsAccountCooling(ctx, store, account.ID()); cooling {
					continue
				}
			}
			key := degradeJob{accountID: account.ID(), route: route}.key()
			rt.mu.Lock()
			due, known := rt.sampled[key]
			if !known {
				due = now.Add(time.Duration(rand.Int64N(int64(interval))))
				rt.sampled[key] = due
			}
			rt.mu.Unlock()
			if known && !due.After(now) && h.EnqueueDegradeProbe(account.ID(), route, DegradeTriggerScheduled) {
				rt.mu.Lock()
				rt.sampled[key] = now.Add(interval)
				rt.mu.Unlock()
			}
		}
	}
}

// DegradeProbeResult is the outcome of one pelican probe.
type DegradeProbeResult struct {
	database.DegradeProbe
	Threshold int `json:"threshold"`
}

var errDegradeNativeForbidden = errors.New("该账号已启用 BPS 且未显式开启原生路由，不能走原生路由探测")

// checkDegradeRoute reports why route of account cannot be probed, or nil.
func checkDegradeRoute(account *auth.Account, route string) error {
	switch {
	case account == nil:
		return errors.New("账号不存在")
	case account.IsRelayStyle() || account.IsCodexAgentIdentity():
		return errors.New("该账号类型不支持降智检测")
	case route == RouteBPS && !account.CodexBPSEligible():
		return errors.New("该账号类型不支持 BPS")
	case route == RouteNative && bpsServedAccount(account) && !account.CodexNativeRouteExplicit():
		// Native traffic is off limits for a BPS account without an
		// explicit native route.
		return errDegradeNativeForbidden
	case route != RouteNative && route != RouteBPS:
		return errors.New("路由必须是 native 或 bps")
	}
	return nil
}

// DegradeJudgeSettings are the verdict threshold and probe model in effect.
func DegradeJudgeSettings() (threshold int, model string) {
	cfg := currentBPSConfig()
	return cfg.DegradeThreshold(), cfg.DegradeModel()
}

// CheckDegradeRoute is checkDegradeRoute for the admin API.
func CheckDegradeRoute(account *auth.Account, route string) error {
	return checkDegradeRoute(account, route)
}

// runDegradeJob runs one queued probe (one replica per route at a time).
func (h *Handler) runDegradeJob(ctx context.Context, job degradeJob) {
	account := h.store.FindByID(job.accountID)
	if account == nil {
		return
	}
	release, ok := h.claimDegradeProbe(ctx, job.key())
	if !ok {
		return
	}
	defer release()
	h.RunDegradeProbe(ctx, account, job.route, job.trigger)
}

func (h *Handler) claimDegradeProbe(ctx context.Context, key string) (func(), bool) {
	shared := bpsSharedAccountStore(h.bpsCache())
	if shared == nil {
		return func() {}, true
	}
	owner := NewUpstreamSessionUUID()
	leaseCtx, cancel := context.WithTimeout(ctx, bpsAttachmentCacheTimeout)
	acquired, err := shared.AcquireLease(leaseCtx, degradeLeaseNS, key, owner, degradeLeaseTTL)
	cancel()
	if err != nil || !acquired {
		return nil, false
	}
	return func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bpsAttachmentCacheTimeout)
		_ = shared.ReleaseLease(releaseCtx, degradeLeaseNS, key, owner)
		cancel()
	}, true
}

// RunDegradeProbe sends the pelican prompt to route of account (one retry on
// an invalid sample), stores the probe and applies the verdict to the route
// breaker: degraded breaks the route (or climbs a tier), ok closes an open
// one, invalid never counts (a recovery retries a minute later).
func (h *Handler) RunDegradeProbe(ctx context.Context, account *auth.Account, route, trigger string) DegradeProbeResult {
	cfg := currentBPSConfig()
	model, threshold := cfg.DegradeModel(), cfg.DegradeThreshold()
	result := DegradeProbeResult{DegradeProbe: database.DegradeProbe{AccountID: account.ID(), Route: route, Model: model, Trigger: trigger, Verdict: degradejudge.VerdictInvalid}, Threshold: threshold}
	started := time.Now()
	if err := checkDegradeRoute(account, route); err != nil {
		result.Error = err.Error()
		return result
	}
	for attempt := 0; attempt < 2; attempt++ {
		text, reported, err := h.executeDegradeProbe(ctx, account, route, model)
		result.UpstreamModel, result.Error = reported, ""
		if err != nil {
			result.Error = truncateProbeText(err.Error())
			var forbidden degradeForbiddenError
			if errors.As(err, &forbidden) && route == RouteNative {
				// A native 403 is a route failure in its own right.
				result.Verdict = degradejudge.VerdictDegraded
				break
			}
			continue
		}
		html, metrics, verdict := degradejudge.Judge(text, threshold)
		result.HTML, result.Score, result.Bytes, result.Verdict = html, metrics.Score, metrics.Bytes, verdict
		if verdict == degradejudge.VerdictInvalid {
			result.Error = "invalid sample: no HTML in the output"
			continue
		}
		if verdict == degradejudge.VerdictOK && nativeModelMismatch(model, model, reported) {
			result.Verdict = degradejudge.VerdictDegraded
			result.Error = "upstream reported " + reported
		}
		break
	}
	result.DurationMs = time.Since(started).Milliseconds()
	if h.db != nil {
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		if err := h.db.InsertDegradeProbe(writeCtx, &result.DegradeProbe); err != nil {
			log.Printf("[degrade] account=%d store probe: %v", account.ID(), err)
		}
		cancel()
	}
	h.applyDegradeVerdict(ctx, account, result)
	log.Printf("[degrade] account=%d %s pelican probe (%s): %s score=%d/%d bytes=%d model=%s reported=%s %s", account.ID(), route, trigger, result.Verdict, result.Score, threshold, result.Bytes, model, result.UpstreamModel, result.Error)
	return result
}

func (h *Handler) applyDegradeVerdict(ctx context.Context, account *auth.Account, result DegradeProbeResult) {
	route := result.Route
	if !degradeBreakerApplies(account, route) {
		return
	}
	open := routeBreakerOpen(ctx, h.bpsCache(), account, route)
	switch result.Verdict {
	case degradejudge.VerdictDegraded:
		detail := fmt.Sprintf("score %d < %d", result.Score, result.Threshold)
		if result.UpstreamModel != "" && nativeModelMismatch(result.Model, result.Model, result.UpstreamModel) {
			detail = result.Model + " → " + result.UpstreamModel
		} else if result.Error != "" && result.Score == 0 {
			detail = result.Error
		}
		h.breakRoute(ctx, account, route, degradeBreakTriggerPelican, detail, result.Model)
	case degradejudge.VerdictOK:
		if open {
			h.closeRoute(ctx, account, route)
		}
	default:
		if open && result.Trigger == DegradeTriggerRecovery {
			now := time.Now()
			bpsAccountStateStore.update(ctx, h.bpsCache(), routeBreakerKey(route, account.ID()), now, func(r *bpsAccountRecord) {
				r.LastProbe, r.LastProbeResult, r.NextProbe = now, "invalid", now.Add(degradeInvalidRetry)
			})
		}
	}
	if result.Verdict != degradejudge.VerdictInvalid {
		now := time.Now()
		bpsAccountStateStore.update(ctx, h.bpsCache(), routeBreakerKey(route, account.ID()), now, func(r *bpsAccountRecord) {
			r.LastProbe, r.LastProbeResult = now, fmt.Sprintf("%s %d", result.Verdict, result.Score)
		})
	}
}

type degradeForbiddenError struct{ status int }

func (e degradeForbiddenError) Error() string { return fmt.Sprintf("HTTP %d", e.status) }

// executeDegradeProbe sends the pelican prompt (internal traffic, not logged
// as client usage) and returns the output text and the reported model.
func (h *Handler) executeDegradeProbe(ctx context.Context, account *auth.Account, route, model string) (string, string, error) {
	payload, _ := json.Marshal(map[string]any{
		"model":        model,
		"input":        []map[string]any{{"role": "user", "content": []map[string]string{{"type": "input_text", "text": degradejudge.Prompt}}}},
		"reasoning":    map[string]string{"effort": degradejudge.ReasoningEffort, "summary": "auto"},
		"stream":       true,
		"store":        false,
		"instructions": "You are a helpful assistant.",
	})
	probeCtx, cancel := context.WithTimeout(ctx, degradeProbeTimeout)
	defer cancel()
	var resp *http.Response
	var err error
	if route == RouteNative {
		resp, err = ExecuteRequest(probeCtx, account, payload, NewUpstreamSessionUUID(), account.GetProxyURL(), "", h.deviceCfg, nil, false)
	} else {
		p, ok := plugins.Default().Get(BPSPluginID)
		if !ok {
			return "", "", errors.New("BPS plugin not registered")
		}
		probeCtx, _ = WithCodexTestMode(probeCtx, "bps")
		req := plugins.NewRequest(NewUpstreamSessionUUID(), plugins.KindResponses, payload, nil, 0)
		req.Model = model
		req.SetState(BPSPluginID, &bpsRequest{handler: h, probe: true})
		resp, err = plugins.Default().RouteFor(p, req, account, model, plugins.KindResponses).Execute(probeCtx, plugins.ReqEnv{Account: account, Model: model, Body: payload, CacheKey: NewUpstreamSessionUUID()})
	}
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		return "", "", degradeForbiddenError{status: resp.StatusCode}
	}
	if resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	observer := &upstreamResponseModelObserver{}
	var text strings.Builder
	scanner := bufio.NewScanner(io.LimitReader(resp.Body, 16<<20))
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(strings.TrimSpace(scanner.Text()), "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "" || data == "[DONE]" {
			continue
		}
		observeUpstreamResponseModelPayload(observer, []byte(data), "")
		var event struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		}
		if json.Unmarshal([]byte(data), &event) == nil && event.Type == "response.output_text.delta" {
			text.WriteString(event.Delta)
		}
	}
	return text.String(), observer.Model(), scanner.Err()
}

// DegradeRouteBrokenStatus reports route breaker state for the admin view.
func DegradeRouteBrokenStatus(ctx context.Context, store cache.TokenCache, account *auth.Account, route string) (open bool, until time.Time, trigger, detail string) {
	if account == nil {
		return false, time.Time{}, "", ""
	}
	now := time.Now()
	record := bpsAccountStateStore.load(ctx, store, routeBreakerKey(route, account.ID()), now)
	if record.Reason != RouteDegradedReason || !record.open(now) {
		return false, time.Time{}, "", ""
	}
	until = record.Until
	if record.NextProbe.After(until) {
		until = record.NextProbe
	}
	return true, until, record.Trigger, record.Detail
}
