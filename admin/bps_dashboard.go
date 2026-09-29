package admin

import (
	"net/http"
	"sort"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/codex2api/proxy/plugins"
	"github.com/gin-gonic/gin"
)

// BPS dashboard: account health, per-account state, traffic and recovery in
// one response for the plugin overview page.

// BPS account states on the dashboard.
const (
	bpsStateActive          = "active"
	bpsStatePolicyBlocked   = "policy_blocked"
	bpsStateRateCooling     = "rate_cooling"
	bpsStateBudgetExhausted = "budget_exhausted"
)

type bpsDashboardSummary struct {
	Total           int  `json:"total"`
	Usable          int  `json:"usable"`
	PolicyBlocked   int  `json:"policy_blocked"`
	RateCooling     int  `json:"rate_cooling"`
	BudgetExhausted int  `json:"budget_exhausted"`
	MinUsable       int  `json:"min_usable"`
	Warning         bool `json:"warning"`
}

type bpsDashboardAccount struct {
	AccountID           int64      `json:"account_id"`
	Name                string     `json:"name"`
	State               string     `json:"state"`
	InFlight            int        `json:"in_flight"`
	MaxConcurrency      int        `json:"max_concurrency"`
	BudgetUsed          int        `json:"budget_used"`
	Budget              int        `json:"budget"`
	BudgetWindowSeconds int64      `json:"budget_window_seconds"`
	Tier                int        `json:"tier,omitempty"`
	Tiers               int        `json:"tiers,omitempty"`
	CoolingUntil        *time.Time `json:"cooling_until,omitempty"`
	BlockedAt           *time.Time `json:"blocked_at,omitempty"`
	ElapsedSeconds      int64      `json:"elapsed_seconds,omitempty"`
	NextProbeAt         *time.Time `json:"next_probe_at,omitempty"`
	LastProbeResult     string     `json:"last_probe_result,omitempty"`
}

type bpsDashboardRecovery struct {
	Blocked              int   `json:"blocked"`
	LongestActiveSeconds int64 `json:"longest_active_seconds"`
	Recovered            int   `json:"recovered"`
	MinSeconds           int64 `json:"min_seconds"`
	MedianSeconds        int64 `json:"median_seconds"`
	MaxSeconds           int64 `json:"max_seconds"`
}

// bpsDashboardState classifies an account's BPS state (policy first, then
// rate cooling, then budget).
func bpsDashboardState(status proxy.BPSAccountStatus, now time.Time) string {
	switch {
	case status.ProbePending || status.Reason == proxy.BPSPolicyBlockedKind && status.CoolingUntil.After(now):
		return bpsStatePolicyBlocked
	case status.Reason == proxy.BPSRateLimitedReason && status.CoolingUntil.After(now):
		return bpsStateRateCooling
	case status.Budget > 0 && status.BudgetUsed >= status.Budget:
		return bpsStateBudgetExhausted
	}
	return bpsStateActive
}

// bpsRecoveryStats computes the recovery panel from active blocks and the
// sorted durations of recovered ones.
func bpsRecoveryStats(active []database.BPSPolicyBlock, durations []int64, now time.Time) bpsDashboardRecovery {
	out := bpsDashboardRecovery{Blocked: len(active), Recovered: len(durations)}
	for _, block := range active {
		out.LongestActiveSeconds = max(out.LongestActiveSeconds, int64(block.Elapsed(now)/time.Second))
	}
	if len(durations) > 0 {
		sorted := append([]int64(nil), durations...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		out.MinSeconds, out.MaxSeconds = sorted[0], sorted[len(sorted)-1]
		if n := len(sorted); n%2 == 1 {
			out.MedianSeconds = sorted[n/2]
		} else {
			out.MedianSeconds = (sorted[n/2-1] + sorted[n/2]) / 2
		}
	}
	return out
}

// GetTransportPluginDashboard returns the BPS dashboard: account health
// summary, per-account state, traffic for the last 1h and 24h, and recovery.
func (h *Handler) GetTransportPluginDashboard(c *gin.Context) {
	p, ok := transportPluginFromParam(c)
	if !ok {
		return
	}
	if p.ID() != proxy.BPSPluginID {
		writeError(c, http.StatusNotFound, "该插件没有仪表盘")
		return
	}
	ctx := c.Request.Context()
	now := time.Now()
	minUsable, window := proxy.BPSDashboardSettings()

	var accounts []*auth.Account
	if h.store != nil {
		for _, account := range h.store.Accounts() {
			if account.CodexBPSEligible() && plugins.Default().EnabledFor(p, account) {
				accounts = append(accounts, account)
			}
		}
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID() < accounts[j].ID() })
	ids := make([]int64, len(accounts))
	for i, account := range accounts {
		ids[i] = account.ID()
	}
	active, _, err := h.db.ListBPSPolicyBlocks(ctx, 1)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	activeByAccount := map[int64]database.BPSPolicyBlock{}
	for _, block := range active {
		activeByAccount[block.AccountID] = block
	}
	summary := bpsDashboardSummary{Total: len(accounts), MinUsable: minUsable}
	rows := make([]bpsDashboardAccount, 0, len(accounts))
	for i, status := range proxy.BPSAccountStatuses(ctx, h.cache, ids) {
		account := accounts[i]
		account.Mu().RLock()
		name := account.Email
		account.Mu().RUnlock()
		row := bpsDashboardAccount{
			AccountID: status.AccountID, Name: name, State: bpsDashboardState(status, now),
			InFlight: status.InFlight, MaxConcurrency: status.MaxConcurrency,
			BudgetUsed: status.BudgetUsed, Budget: status.Budget, BudgetWindowSeconds: int64(window / time.Second),
		}
		switch row.State {
		case bpsStateActive:
			summary.Usable++
		case bpsStatePolicyBlocked:
			summary.PolicyBlocked++
			row.Tier, row.Tiers, row.LastProbeResult = status.PolicyTier, status.PolicyTiers, status.LastProbeResult
			if !status.NextProbeAt.IsZero() {
				next := status.NextProbeAt
				row.NextProbeAt = &next
			}
			if block, ok := activeByAccount[status.AccountID]; ok {
				blockedAt := block.BlockedAt
				row.BlockedAt, row.ElapsedSeconds = &blockedAt, int64(block.Elapsed(now)/time.Second)
				row.Tier = max(row.Tier, block.Tier)
			}
		case bpsStateRateCooling:
			summary.RateCooling++
		case bpsStateBudgetExhausted:
			summary.BudgetExhausted++
		}
		if status.CoolingUntil.After(now) {
			until := status.CoolingUntil
			row.CoolingUntil = &until
		}
		rows = append(rows, row)
	}
	summary.Warning = summary.Usable <= summary.MinUsable

	traffic := map[string]database.TransportTrafficStats{}
	for label, span := range map[string]time.Duration{"1h": time.Hour, "24h": 24 * time.Hour} {
		stats, err := h.db.TransportTrafficStatsSince(ctx, proxy.BPSPluginID, now.Add(-span))
		if err != nil {
			writeInternalError(c, err)
			return
		}
		traffic[label] = stats
	}
	durations, err := h.db.BPSPolicyBlockDurations(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"summary":  summary,
		"accounts": rows,
		"traffic":  traffic,
		"recovery": bpsRecoveryStats(active, durations, now),
		"now":      now.UTC(),
	})
}

// bpsActivityRow is one account on the live activity panel.
type bpsActivityRow struct {
	AccountID      int64      `json:"account_id"`
	Name           string     `json:"name"`
	State          string     `json:"state"`
	Tier           int        `json:"tier,omitempty"`
	Tiers          int        `json:"tiers,omitempty"`
	InFlight       int        `json:"in_flight"`
	MaxConcurrency int        `json:"max_concurrency"`
	Succeeded      int        `json:"succeeded"`
	Attempts       int        `json:"attempts"`
	Budget         int        `json:"budget"`
	CoolingUntil   *time.Time `json:"cooling_until,omitempty"`
	ElapsedSeconds int64      `json:"elapsed_seconds,omitempty"`
	NextProbeAt    *time.Time `json:"next_probe_at,omitempty"`
	LastRequestAt  *time.Time `json:"last_request_at,omitempty"`
}

// sortBPSActivity puts accounts with requests in flight first (most first),
// then the busiest by successful requests in the window.
func sortBPSActivity(rows []bpsActivityRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].InFlight != rows[j].InFlight {
			return rows[i].InFlight > rows[j].InFlight
		}
		if rows[i].Succeeded != rows[j].Succeeded {
			return rows[i].Succeeded > rows[j].Succeeded
		}
		return rows[i].AccountID < rows[j].AccountID
	})
}

// GetTransportPluginActivity is the light, frequently polled live view of
// every BPS account: in-flight requests (this replica), successful and total
// requests in the budget window (all replicas), state and last request.
func (h *Handler) GetTransportPluginActivity(c *gin.Context) {
	p, ok := transportPluginFromParam(c)
	if !ok {
		return
	}
	if p.ID() != proxy.BPSPluginID {
		writeError(c, http.StatusNotFound, "该插件没有活动面板")
		return
	}
	ctx := c.Request.Context()
	now := time.Now()
	_, window := proxy.BPSDashboardSettings()
	var accounts []*auth.Account
	if h.store != nil {
		for _, account := range h.store.Accounts() {
			if account.CodexBPSEligible() && plugins.Default().EnabledFor(p, account) {
				accounts = append(accounts, account)
			}
		}
	}
	ids := make([]int64, len(accounts))
	for i, account := range accounts {
		ids[i] = account.ID()
	}
	active, _, err := h.db.ListBPSPolicyBlocks(ctx, 1)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	blockedAt := map[int64]database.BPSPolicyBlock{}
	for _, block := range active {
		blockedAt[block.AccountID] = block
	}
	rows := make([]bpsActivityRow, 0, len(accounts))
	for i, status := range proxy.BPSAccountStatuses(ctx, h.cache, ids) {
		account := accounts[i]
		account.Mu().RLock()
		name := account.Email
		account.Mu().RUnlock()
		row := bpsActivityRow{
			AccountID: status.AccountID, Name: name, State: bpsDashboardState(status, now),
			InFlight: status.InFlight, MaxConcurrency: status.MaxConcurrency,
			Succeeded: status.BudgetUsed, Attempts: status.Attempts, Budget: status.Budget,
		}
		if status.CoolingUntil.After(now) {
			until := status.CoolingUntil
			row.CoolingUntil = &until
		}
		if !status.LastRequestAt.IsZero() {
			last := status.LastRequestAt
			row.LastRequestAt = &last
		}
		if row.State == bpsStatePolicyBlocked {
			row.Tier, row.Tiers = status.PolicyTier, status.PolicyTiers
			if !status.NextProbeAt.IsZero() {
				next := status.NextProbeAt
				row.NextProbeAt = &next
			}
			if block, ok := blockedAt[status.AccountID]; ok {
				row.ElapsedSeconds = int64(block.Elapsed(now) / time.Second)
				row.Tier = max(row.Tier, block.Tier)
			}
		}
		rows = append(rows, row)
	}
	sortBPSActivity(rows)
	c.JSON(http.StatusOK, gin.H{
		"accounts":              rows,
		"window_seconds":        int64(window / time.Second),
		"in_flight_per_replica": true,
		"now":                   now.UTC(),
	})
}
