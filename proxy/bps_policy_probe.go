package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy/plugins"
)

// Usage-policy recovery probes. A policy-cooling account never receives real
// client traffic, not even when its tier expires: the cooldown ends only when
// a background synthetic probe (a tiny BPS request, not logged as client
// traffic) succeeds. A probe blocked by the usage policy climbs to the next
// tier; any other probe failure is retried a minute later.

const (
	bpsProbeNamespace  = "bps-policy-probe-v1"
	bpsProbeTimeout    = 60 * time.Second
	bpsProbeRetryDelay = time.Minute
	bpsProbeFallback   = "gpt-6-sol"

	bpsProbeOK      = "ok"
	bpsProbeBlocked = "blocked"
)

// bpsProbeInterval is how often due probes are looked for (a variable for tests).
var bpsProbeInterval = 20 * time.Second

// StartBPSPolicyProber runs due usage-policy probes until ctx ends.
func (h *Handler) StartBPSPolicyProber(ctx context.Context) {
	if h == nil || h.store == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(bpsProbeInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				h.runBPSPolicyProbes(ctx, now)
			}
		}
	}()
}

// runBPSPolicyProbes probes every BPS account whose usage-policy tier has
// expired and whose probe is due; one replica probes an account at a time.
func (h *Handler) runBPSPolicyProbes(ctx context.Context, now time.Time) {
	p, ok := plugins.Default().Get(BPSPluginID)
	if !ok {
		return
	}
	store := h.bpsCache()
	for _, account := range h.store.Accounts() {
		if ctx.Err() != nil {
			return
		}
		if !account.CodexBPSEligible() || !plugins.Default().EnabledFor(p, account) {
			continue
		}
		record := bpsAccountStateStore.load(ctx, store, bpsAccountStateKey(account.ID()), now)
		if record.Reason != BPSPolicyBlockedKind || !record.NeedsProbe || record.active(now) || record.NextProbe.After(now) {
			continue
		}
		if release, ok := h.claimRouteProbe(ctx, fmt.Sprintf("account:%d", account.ID())); ok {
			h.probeBPSPolicyAccount(ctx, account)
			release()
		}
	}
	h.runNativeRouteProbes(ctx, now)
}

// claimRouteProbe takes a short lease so only one replica probes a route
// (key: account:<id> for BPS, native:<id> for the native route).
func (h *Handler) claimRouteProbe(ctx context.Context, key string) (func(), bool) {
	shared := bpsSharedAccountStore(h.bpsCache())
	if shared == nil {
		return func() {}, true
	}
	owner := NewUpstreamSessionUUID()
	leaseCtx, cancel := context.WithTimeout(ctx, bpsAttachmentCacheTimeout)
	acquired, err := shared.AcquireLease(leaseCtx, bpsProbeNamespace, key, owner, bpsProbeTimeout+10*time.Second)
	cancel()
	if err != nil || !acquired {
		return nil, false
	}
	return func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bpsAttachmentCacheTimeout)
		_ = shared.ReleaseLease(releaseCtx, bpsProbeNamespace, key, owner)
		cancel()
	}, true
}

// bpsProbeModel is the model the probe asks for.
func (h *Handler) bpsProbeModel() string {
	cfg := currentBPSConfig()
	if model := strings.TrimSpace(cfg.ProbeModel); model != "" {
		return model
	}
	if model := strings.TrimSpace(h.store.GetTestModel()); model != "" && cfg.ServesModel(model) {
		return model
	}
	return bpsProbeFallback
}

// probeBPSPolicyAccount sends one probe and applies its outcome.
func (h *Handler) probeBPSPolicyAccount(ctx context.Context, account *auth.Account) string {
	result := h.executeBPSProbe(ctx, account)
	now := time.Now()
	cfg := currentBPSConfig()
	ladder := cfg.PolicyLadder()
	record := bpsAccountStateStore.update(ctx, h.bpsCache(), bpsAccountStateKey(account.ID()), now, func(r *bpsAccountRecord) {
		r.LastProbe, r.LastProbeResult = now, result
		switch result {
		case bpsProbeOK:
			r.Until, r.Reason, r.NeedsProbe, r.NextProbe = time.Time{}, "", false, time.Time{}
		case bpsProbeBlocked:
			r.PolicyTier = min(r.PolicyTier+1, len(ladder))
			r.LastPolicyBlock = now
			r.Until, r.Reason, r.NeedsProbe, r.NextProbe = now.Add(ladder[max(r.PolicyTier, 1)-1]), BPSPolicyBlockedKind, true, time.Time{}
		default:
			r.NextProbe = now.Add(bpsProbeRetryDelay)
		}
	})
	h.recordBPSProbeHistory(ctx, account.ID(), now, result, record)
	switch result {
	case bpsProbeOK:
		log.Printf("[bps] account=%d usage-policy probe succeeded; the account serves BPS again (tier %d kept for decay)", account.ID(), record.PolicyTier)
	case bpsProbeBlocked:
		log.Printf("[bps] account=%d usage-policy probe still blocked; policy tier %d/%d, next probe after %s", account.ID(), record.PolicyTier, len(ladder), record.Until.Format(time.RFC3339))
	default:
		log.Printf("[bps] account=%d usage-policy probe inconclusive (%s); retrying in %s", account.ID(), result, bpsProbeRetryDelay)
	}
	return result
}

// executeBPSProbe sends a tiny BPS request on account and classifies it:
// ok, blocked (usage policy) or "error: ...". It bypasses the account's
// cooldown (it is the probe) and is not logged as client traffic.
func (h *Handler) executeBPSProbe(ctx context.Context, account *auth.Account) string {
	p, ok := plugins.Default().Get(BPSPluginID)
	if !ok {
		return "error: BPS plugin not registered"
	}
	model := h.bpsProbeModel()
	payload, _ := json.Marshal(map[string]any{"model": model, "input": "ping", "stream": true, "reasoning": map[string]string{"effort": "low"}})
	probeCtx, cancel := context.WithTimeout(ctx, bpsProbeTimeout)
	defer cancel()
	probeCtx, _ = WithCodexTestMode(probeCtx, "bps")
	req := plugins.NewRequest(NewUpstreamSessionUUID(), plugins.KindResponses, payload, nil, 0)
	req.Model = model
	state := &bpsRequest{handler: h, probe: true}
	req.SetState(BPSPluginID, state)
	route := plugins.Default().RouteFor(p, req, account, model, plugins.KindResponses)
	resp, err := route.Execute(probeCtx, plugins.ReqEnv{Account: account, Model: model, Body: payload, CacheKey: NewUpstreamSessionUUID()})
	if err != nil {
		return "error: " + truncateProbeText(err.Error())
	}
	_, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	state.mu.Lock()
	class := state.probeClass
	state.mu.Unlock()
	switch {
	case class == BPSPolicyBlockedKind:
		return bpsProbeBlocked
	case class != "":
		return "error: " + class
	case resp.StatusCode >= 300:
		return fmt.Sprintf("error: HTTP %d", resp.StatusCode)
	case readErr != nil:
		return "error: " + truncateProbeText(readErr.Error())
	}
	return bpsProbeOK
}

func truncateProbeText(text string) string {
	if len(text) > 120 {
		return text[:120]
	}
	return text
}

// recordBPSProbeHistory updates the account's block history row: every probe
// is counted, and the first successful probe closes the block and records how
// long it lasted (the recovery time).
func (h *Handler) recordBPSProbeHistory(ctx context.Context, accountID int64, now time.Time, result string, record bpsAccountRecord) {
	if h == nil || h.db == nil {
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := h.db.RecordBPSPolicyProbe(writeCtx, accountID, now, result, record.PolicyTier); err != nil {
		log.Printf("[bps] account=%d record policy probe: %v", accountID, err)
	}
	if result != bpsProbeOK {
		return
	}
	duration, found, err := h.db.ClearBPSPolicyBlock(writeCtx, accountID, now)
	if err != nil {
		log.Printf("[bps] account=%d close policy block: %v", accountID, err)
		return
	}
	if found {
		log.Printf("[bps] account=%d RECOVERED from the usage-policy block after %s", accountID, duration.Round(time.Second))
	}
}
