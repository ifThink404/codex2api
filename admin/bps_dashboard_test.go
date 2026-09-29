package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestBPSDashboardStateClassification(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name   string
		status proxy.BPSAccountStatus
		want   string
	}{
		{name: "active", status: proxy.BPSAccountStatus{}, want: bpsStateActive},
		{name: "policy cooling", status: proxy.BPSAccountStatus{Reason: proxy.BPSPolicyBlockedKind, CoolingUntil: now.Add(time.Hour)}, want: bpsStatePolicyBlocked},
		{name: "policy waiting for its probe", status: proxy.BPSAccountStatus{ProbePending: true}, want: bpsStatePolicyBlocked},
		{name: "rate cooling", status: proxy.BPSAccountStatus{Reason: proxy.BPSRateLimitedReason, CoolingUntil: now.Add(time.Minute)}, want: bpsStateRateCooling},
		{name: "rate cooldown over", status: proxy.BPSAccountStatus{Reason: proxy.BPSRateLimitedReason, CoolingUntil: now.Add(-time.Minute)}, want: bpsStateActive},
		{name: "budget exhausted", status: proxy.BPSAccountStatus{Budget: 1250, BudgetUsed: 1250}, want: bpsStateBudgetExhausted},
		{name: "budget left", status: proxy.BPSAccountStatus{Budget: 1250, BudgetUsed: 1249}, want: bpsStateActive},
		{name: "policy wins over budget", status: proxy.BPSAccountStatus{ProbePending: true, Budget: 10, BudgetUsed: 10}, want: bpsStatePolicyBlocked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := bpsDashboardState(tc.status, now); got != tc.want {
				t.Fatalf("state = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestBPSRecoveryStats(t *testing.T) {
	now := time.Now()
	active := []database.BPSPolicyBlock{{BlockedAt: now.Add(-2 * time.Hour)}, {BlockedAt: now.Add(-270 * time.Minute)}}
	odd := bpsRecoveryStats(active, []int64{600, 60, 3600}, now)
	if odd.Blocked != 2 || odd.LongestActiveSeconds < 270*60-1 || odd.Recovered != 3 || odd.MinSeconds != 60 || odd.MedianSeconds != 600 || odd.MaxSeconds != 3600 {
		t.Fatalf("odd = %+v", odd)
	}
	even := bpsRecoveryStats(nil, []int64{100, 200, 300, 400}, now)
	if even.MedianSeconds != 250 || even.LongestActiveSeconds != 0 {
		t.Fatalf("even = %+v", even)
	}
	if empty := bpsRecoveryStats(nil, nil, now); empty.Recovered != 0 || empty.MedianSeconds != 0 {
		t.Fatalf("empty = %+v", empty)
	}
}

func TestBPSDashboardEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	if err := db.MigrateBPSPlugin(context.Background()); err != nil {
		t.Fatal(err)
	}
	db.SetUsageLogConfig(database.UsageLogModeFull, 100, 300)
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	on, off := true, false
	for id, enabled := range map[int64]*bool{51: &on, 52: &off} {
		store.AddAccount(&auth.Account{DBID: id, Email: "bps@example.com", AccessToken: "at", Status: auth.StatusReady})
		store.ApplyAccountTransportPluginOverride(id, proxy.BPSPluginID, enabled)
	}
	store.AddAccount(&auth.Account{DBID: 53, Email: "disabled@example.com", AccessToken: "at", Status: auth.StatusReady, DispatchPaused: 1})
	store.ApplyAccountTransportPluginOverride(53, proxy.BPSPluginID, &on)
	store.AddAccount(&auth.Account{DBID: 54, Email: "unauthorized@example.com", AccessToken: "at", Status: auth.StatusReady, Disabled: 1})
	store.ApplyAccountTransportPluginOverride(54, proxy.BPSPluginID, &on)
	store.AddAccount(&auth.Account{DBID: 55, Email: "error@example.com", AccessToken: "at", Status: auth.StatusError})
	store.ApplyAccountTransportPluginOverride(55, proxy.BPSPluginID, &on)
	h := &Handler{db: db, store: store}
	router := gin.New()
	h.registerTransportPluginRoutes(router.Group("/api/admin"))
	ctx := context.Background()
	for _, row := range []database.UsageLogInput{
		{AccountID: 51, Transport: "bps", StatusCode: 200, FirstTokenMs: 800},
		{AccountID: 51, Transport: "bps", StatusCode: 200, FirstTokenMs: 1200},
		{AccountID: 51, Transport: "bps", StatusCode: 429, PluginMeta: `{"rate_limit_scope":"org"}`, UpstreamErrorKind: "rate_limited"},
		{AccountID: 51, Transport: "bps", StatusCode: 429, PluginMeta: `{"rate_limit_scope":"account"}`, UpstreamErrorKind: "rate_limited"},
		{AccountID: 51, Transport: "bps", StatusCode: 403, UpstreamErrorKind: "bps_policy_blocked"},
		{AccountID: 51, Transport: "bps", StatusCode: 200, InternalReason: "connection_test"},
		{AccountID: 52, Transport: "native", StatusCode: 200},
	} {
		input := row
		if err := db.InsertUsageLog(ctx, &input); err != nil {
			t.Fatal(err)
		}
	}
	db.FlushUsageLogs()
	if err := db.OpenBPSPolicyBlock(ctx, 60, time.Now().Add(-3*time.Hour), 2); err != nil {
		t.Fatal(err)
	}
	if err := db.OpenBPSPolicyBlock(ctx, 61, time.Now().Add(-5*time.Hour), 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ClearBPSPolicyBlock(ctx, 61, time.Now().Add(-4*time.Hour)); err != nil {
		t.Fatal(err)
	}

	rec := doTransportPluginRequest(t, router, http.MethodGet, "/api/admin/plugins/bps/dashboard", "")
	var out struct {
		Accounts []struct {
			AccountID int64  `json:"account_id"`
			State     string `json:"state"`
		} `json:"accounts"`
		Traffic  map[string]database.TransportTrafficStats   `json:"traffic"`
		Timeline map[string][]database.TransportTrafficPoint `json:"timeline"`
		Recovery struct {
			Blocked              int   `json:"blocked"`
			LongestActiveSeconds int64 `json:"longest_active_seconds"`
			Recovered            int   `json:"recovered"`
			MedianSeconds        int64 `json:"median_seconds"`
		} `json:"recovery"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("dashboard: %d %s", rec.Code, rec.Body.String())
	}
	var summary struct {
		Total     int  `json:"total"`
		Disabled  int  `json:"disabled"`
		Invalid   int  `json:"invalid"`
		Usable    int  `json:"usable"`
		MinUsable int  `json:"min_usable"`
		Warning   bool `json:"warning"`
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	_ = json.Unmarshal(raw["summary"], &summary)
	if summary.Total != 1 || summary.Usable != 1 || summary.Disabled != 1 || summary.Invalid != 2 || summary.MinUsable != 2 || !summary.Warning {
		t.Fatalf("summary = %+v (only the enabled, working BPS account counts; disabled and 401/error ones only as such; 1 usable <= floor 2 warns)", summary)
	}
	if len(out.Accounts) != 1 || out.Accounts[0].AccountID != 51 || out.Accounts[0].State != bpsStateActive {
		t.Fatalf("accounts = %+v", out.Accounts)
	}
	day := out.Traffic["24h"]
	if day.Requests != 5 || day.Succeeded != 2 || day.OrgRateLimited != 1 || day.RateLimited != 1 || day.PolicyBlocked != 1 || day.AvgFirstTokenMs != 1000 || day.InternalRequests != 1 {
		t.Fatalf("24h traffic = %+v", day)
	}
	if out.Traffic["1h"].Requests != 5 || day.SuccessRate < 0.39 || day.SuccessRate > 0.41 {
		t.Fatalf("1h traffic = %+v / rate %v", out.Traffic["1h"], day.SuccessRate)
	}
	for _, label := range []string{"1h", "24h"} {
		var sum database.TransportTrafficPoint
		for _, point := range out.Timeline[label] {
			if _, err := time.Parse(time.RFC3339, point.Bucket); err != nil {
				t.Fatalf("%s bucket %q: %v", label, point.Bucket, err)
			}
			sum.Requests += point.Requests
			sum.Succeeded += point.Succeeded
			sum.Errors4xx += point.Errors4xx
			sum.OrgRateLimited += point.OrgRateLimited
			sum.RateLimited += point.RateLimited
			sum.PolicyBlocked += point.PolicyBlocked
		}
		if sum.Requests != 5 || sum.Succeeded != 2 || sum.Errors4xx != 3 || sum.OrgRateLimited != 1 || sum.RateLimited != 1 || sum.PolicyBlocked != 1 {
			t.Fatalf("%s timeline sums to %+v (client rows only, like the totals)", label, sum)
		}
	}
	if out.Recovery.Blocked != 1 || out.Recovery.LongestActiveSeconds < 3*3600-5 || out.Recovery.Recovered != 1 || out.Recovery.MedianSeconds != 3600 {
		t.Fatalf("recovery = %+v", out.Recovery)
	}
	if rec := doTransportPluginRequest(t, router, http.MethodGet, "/api/admin/plugins/missing/dashboard", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown plugin: %d", rec.Code)
	}
}

func TestBPSActivitySortPutsBusyAccountsFirst(t *testing.T) {
	rows := []bpsActivityRow{
		{AccountID: 1, InFlight: 0, Succeeded: 900},
		{AccountID: 2, InFlight: 3, Succeeded: 10},
		{AccountID: 3, InFlight: 0, Succeeded: 1200},
		{AccountID: 4, InFlight: 7, Succeeded: 5},
		{AccountID: 5, InFlight: 0, Succeeded: 900},
	}
	sortBPSActivity(rows)
	var order []int64
	for _, row := range rows {
		order = append(order, row.AccountID)
	}
	want := []int64{4, 2, 3, 1, 5}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v (in flight desc, then successes desc, then id)", order, want)
		}
	}
}

func TestBPSActivityEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	if err := db.MigrateBPSPlugin(context.Background()); err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	on := true
	store.AddAccount(&auth.Account{DBID: 71, Email: "live@example.com", AccessToken: "at", Status: auth.StatusReady})
	store.ApplyAccountTransportPluginOverride(71, proxy.BPSPluginID, &on)
	store.AddAccount(&auth.Account{DBID: 72, Email: "native@example.com", AccessToken: "at", Status: auth.StatusReady})
	store.AddAccount(&auth.Account{DBID: 73, Email: "disabled@example.com", AccessToken: "at", Status: auth.StatusReady, DispatchPaused: 1})
	store.ApplyAccountTransportPluginOverride(73, proxy.BPSPluginID, &on)
	store.AddAccount(&auth.Account{DBID: 74, Email: "unauthorized@example.com", AccessToken: "at", Status: auth.StatusReady, Disabled: 1})
	store.ApplyAccountTransportPluginOverride(74, proxy.BPSPluginID, &on)
	h := &Handler{db: db, store: store}
	router := gin.New()
	h.registerTransportPluginRoutes(router.Group("/api/admin"))
	rec := doTransportPluginRequest(t, router, http.MethodGet, "/api/admin/plugins/bps/activity", "")
	var out struct {
		Accounts []struct {
			AccountID int64  `json:"account_id"`
			Name      string `json:"name"`
			State     string `json:"state"`
			InFlight  int    `json:"in_flight"`
		} `json:"accounts"`
		WindowSeconds      int64 `json:"window_seconds"`
		InFlightPerReplica bool  `json:"in_flight_per_replica"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("activity: %d %s", rec.Code, rec.Body.String())
	}
	if len(out.Accounts) != 1 || out.Accounts[0].AccountID != 71 || out.Accounts[0].Name != "live@example.com" || out.Accounts[0].State != bpsStateActive {
		t.Fatalf("accounts = %+v", out.Accounts)
	}
	if out.WindowSeconds != 24*3600 || !out.InFlightPerReplica {
		t.Fatalf("window = %d per-replica = %v", out.WindowSeconds, out.InFlightPerReplica)
	}
}

func TestCodexBPSActiveNeedsEnabledAccount(t *testing.T) {
	db := newTestAdminDB(t)
	if err := db.MigrateBPSPlugin(context.Background()); err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	on := true
	store.AddAccount(&auth.Account{DBID: 81, Email: "bps@example.com", AccessToken: "at", Status: auth.StatusReady})
	store.ApplyAccountTransportPluginOverride(81, proxy.BPSPluginID, &on)
	live := store.FindByID(81)
	if !codexBPSAccountViewFromRow(&database.AccountRow{ID: 81, Enabled: true}, live).Active {
		t.Fatal("an enabled account with BPS forced on is served by BPS")
	}
	if codexBPSAccountViewFromRow(&database.AccountRow{ID: 81, Enabled: false}, live).Active {
		t.Fatal("a disabled account is not served by BPS")
	}
	atomic.StoreInt32(&live.Disabled, 1)
	if view := codexBPSAccountViewFromRow(&database.AccountRow{ID: 81, Enabled: true}, live); view.Active || !view.CredentialInvalid {
		t.Fatalf("a 401 account is not served by BPS and is flagged invalid: %+v", view)
	}
}
