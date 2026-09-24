package database

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDefaultUpstreamPriorityDoesNotRaiseStoredChargesOrFastCounts(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "default-priority.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := t.Context()
	for index, sample := range []struct{ requested, actual, billing, want string }{
		{"", "priority", "", "default"},
		{"priority", "default", "default", "default"},
		{"priority", "priority", "priority", "priority"},
		{"", "", "", "fast"}, // legacy callers only provided service_tier
	} {
		input := &UsageLogInput{AccountID: int64(91001 + index), APIKeyID: 1, Endpoint: "/v1/responses", Model: "gpt-5.6-sol", StatusCode: 200, InputTokens: 1000, OutputTokens: 100, ServiceTier: "fast", RequestedServiceTier: sample.requested, ActualServiceTier: sample.actual, BillingServiceTier: sample.billing}
		want := CalculateCost(1000, 100, 0, input.Model, sample.want)
		require.Equal(t, want, UsageLogBilledCost(input), "in-memory quota must honor base billing")
		require.NoError(t, db.InsertUsageLog(ctx, input))
	}
	db.FlushUsageLogs()
	logs, err := db.ListRecentUsageLogs(ctx, 10)
	require.NoError(t, err)
	require.Len(t, logs, 4)
	base := CalculateCost(1000, 100, 0, "gpt-5.6-sol", "default")
	for _, log := range logs {
		if log.AccountID == 91001 {
			require.Equal(t, "default", log.BillingServiceTier)
			require.Equal(t, "priority", log.ActualServiceTier)
			require.Equal(t, base, log.AccountBilled)
			require.Equal(t, base, log.UserBilled)
			require.Equal(t, base, log.TotalCost)
		}
	}
	// An old row with separate actual-tier data and an empty base-tier field
	// must still display and filter as standard, without rewriting its money.
	_, err = db.conn.ExecContext(ctx, `UPDATE usage_logs SET billing_service_tier='' WHERE account_id=91001`)
	require.NoError(t, err)
	start, end := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	stats, err := db.GetUsageStats(ctx, start, end, "")
	require.NoError(t, err)
	require.EqualValues(t, 2, stats.FeatureStats.FastRequests)
	fast := true
	page, err := db.ListUsageLogsByTimeRangePaged(ctx, UsageLogFilter{Start: start, End: end, FastOnly: &fast, Page: 1, PageSize: 10})
	require.NoError(t, err)
	require.EqualValues(t, 2, page.Total)
	self, _, _, _, err := db.listAPIKeySelfRecentLogs(ctx, 1, start, end, 1, 10)
	require.NoError(t, err)
	require.Len(t, self, 4)
	standard := 0
	for _, log := range self {
		if log.ServiceTier == "default" {
			standard++
		}
	}
	require.Equal(t, 2, standard)
	logs, err = db.ListRecentUsageLogs(ctx, 10)
	require.NoError(t, err)
	for _, log := range logs {
		if log.AccountID == 91001 {
			require.Equal(t, base, log.TotalCost)
			require.Equal(t, base, log.UserBilled)
		}
	}
}
