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

// TransportTrafficPoint is one time bucket of a transport's client traffic.
type TransportTrafficPoint struct {
	Bucket         string `json:"bucket"`
	Requests       int64  `json:"requests"`
	Succeeded      int64  `json:"succeeded"`
	Errors4xx      int64  `json:"errors_4xx"`
	Errors5xx      int64  `json:"errors_5xx"`
	OrgRateLimited int64  `json:"org_rate_limited"`
	RateLimited    int64  `json:"rate_limited"`
	PolicyBlocked  int64  `json:"policy_blocked"`
}

// TransportTrafficTimeline buckets the transport's client usage rows since
// since into bucketMinutes-wide buckets (UTC epoch aligned, RFC 3339 bucket
// starts), counted like TransportTrafficStatsSince. Empty buckets are left
// out.
func (db *DB) TransportTrafficTimeline(ctx context.Context, transport string, since time.Time, bucketMinutes int) ([]TransportTrafficPoint, error) {
	out := []TransportTrafficPoint{}
	if db == nil || db.conn == nil {
		return out, nil
	}
	if bucketMinutes < 1 {
		bucketMinutes = 1
	}
	bucket := `CAST(FLOOR(EXTRACT(EPOCH FROM created_at) / $3) * $3 AS BIGINT)`
	if db.isSQLite() {
		bucket = `(CAST(strftime('%s', created_at) AS INTEGER) / $3) * $3`
	}
	const success = `status_code < 400 AND COALESCE(upstream_error_kind, '') = ''`
	rows, err := db.conn.QueryContext(ctx, `SELECT `+bucket+` AS bucket,
			COUNT(*),
			COALESCE(SUM(CASE WHEN `+success+` THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status_code >= 400 AND status_code < 500 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status_code >= 500 AND status_code < 600 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN plugin_meta LIKE '%"rate_limit_scope":"org"%' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN plugin_meta LIKE '%"rate_limit_scope":"account"%' OR (status_code = 429 AND plugin_meta NOT LIKE '%"rate_limit_scope":%') THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN upstream_error_kind = 'bps_policy_blocked' THEN 1 ELSE 0 END), 0)
		FROM usage_logs WHERE transport = $1 AND created_at >= $2 AND COALESCE(internal_reason, '') = ''
		GROUP BY 1 ORDER BY 1`, transport, db.timeArg(since.UTC()), bucketMinutes*60)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var epoch int64
		var point TransportTrafficPoint
		if err := rows.Scan(&epoch, &point.Requests, &point.Succeeded, &point.Errors4xx, &point.Errors5xx, &point.OrgRateLimited, &point.RateLimited, &point.PolicyBlocked); err != nil {
			return nil, err
		}
		point.Bucket = time.Unix(epoch, 0).UTC().Format(time.RFC3339)
		out = append(out, point)
	}
	return out, rows.Err()
}

// BPSPolicyBlockDurations returns the durations (seconds) of the kept
// cleared BPS-route blocks, for recovery statistics.
func (db *DB) BPSPolicyBlockDurations(ctx context.Context) ([]int64, error) {
	if db == nil || db.conn == nil {
		return nil, nil
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT duration_seconds FROM bps_policy_blocks WHERE cleared_at IS NOT NULL AND route = 'bps' ORDER BY duration_seconds`)
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
