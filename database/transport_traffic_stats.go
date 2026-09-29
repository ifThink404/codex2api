package database

import (
	"context"
	"time"
)

// TransportTrafficStats summarizes one transport's usage rows since a time.
type TransportTrafficStats struct {
	Requests         int64   `json:"requests"`
	Succeeded        int64   `json:"succeeded"`
	SuccessRate      float64 `json:"success_rate"`
	OrgRateLimited   int64   `json:"org_rate_limited"`
	RateLimited      int64   `json:"rate_limited"`
	PolicyBlocked    int64   `json:"policy_blocked"`
	AvgFirstTokenMs  int64   `json:"avg_first_token_ms"`
	InternalRequests int64   `json:"internal_requests"`
}

// TransportTrafficStatsSince counts the transport's usage rows since since:
// successes (status < 400 without an error kind), organization-level and
// per-account rate limits (plugin_meta rate_limit_scope, or a 429 without
// one), usage-policy blocks and the mean first-token time of successes.
// Only client rows count; internal rows (connection tests) are counted
// separately.
func (db *DB) TransportTrafficStatsSince(ctx context.Context, transport string, since time.Time) (TransportTrafficStats, error) {
	var stats TransportTrafficStats
	if db == nil || db.conn == nil {
		return stats, nil
	}
	var firstTokenSum, firstTokenCount int64
	const client = `COALESCE(internal_reason, '') = ''`
	const success = `status_code < 400 AND COALESCE(upstream_error_kind, '') = ''`
	err := db.conn.QueryRowContext(ctx, `SELECT
			COALESCE(SUM(CASE WHEN `+client+` THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN `+client+` AND `+success+` THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN `+client+` AND plugin_meta LIKE '%"rate_limit_scope":"org"%' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN `+client+` AND (plugin_meta LIKE '%"rate_limit_scope":"account"%' OR (status_code = 429 AND plugin_meta NOT LIKE '%"rate_limit_scope":%')) THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN `+client+` AND upstream_error_kind = 'bps_policy_blocked' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN `+client+` AND `+success+` AND first_token_ms > 0 THEN first_token_ms ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN `+client+` AND `+success+` AND first_token_ms > 0 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN `+client+` THEN 0 ELSE 1 END), 0)
		FROM usage_logs WHERE transport = $1 AND created_at >= $2`, transport, db.timeArg(since.UTC())).Scan(
		&stats.Requests, &stats.Succeeded, &stats.OrgRateLimited, &stats.RateLimited, &stats.PolicyBlocked, &firstTokenSum, &firstTokenCount, &stats.InternalRequests)
	if err != nil {
		return stats, err
	}
	if stats.Requests > 0 {
		stats.SuccessRate = float64(stats.Succeeded) / float64(stats.Requests)
	}
	if firstTokenCount > 0 {
		stats.AvgFirstTokenMs = firstTokenSum / firstTokenCount
	}
	return stats, nil
}

// BPSPolicyBlockDurations returns the durations (seconds) of the kept
// cleared blocks, for recovery statistics.
func (db *DB) BPSPolicyBlockDurations(ctx context.Context) ([]int64, error) {
	if db == nil || db.conn == nil {
		return nil, nil
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT duration_seconds FROM bps_policy_blocks WHERE cleared_at IS NOT NULL ORDER BY duration_seconds`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var seconds int64
		if err := rows.Scan(&seconds); err != nil {
			return nil, err
		}
		out = append(out, seconds)
	}
	return out, rows.Err()
}
