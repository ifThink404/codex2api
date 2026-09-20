package database

import (
	"context"
	"time"
)

const UsageClientUserAgentSampleLimit = 100000

type UsageClientUserAgentSample struct {
	UserAgent string
	Count     int64
	LastSeen  time.Time
}

// Read only persisted inbound UAs. Limit the input before filtering/grouping so
// missing UAs cannot turn a preview into an unbounded historical scan.
func (db *DB) RecentUsageClientUserAgents(ctx context.Context) ([]UsageClientUserAgentSample, error) {
	return db.recentUsageClientUserAgents(ctx, UsageClientUserAgentSampleLimit)
}

func (db *DB) recentUsageClientUserAgents(ctx context.Context, limit int) ([]UsageClientUserAgentSample, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT client_user_agent, COUNT(*), MAX(created_at)
		FROM (SELECT client_user_agent, created_at, internal_reason, is_retry_attempt
		      FROM usage_logs ORDER BY created_at DESC LIMIT $1) recent
		WHERE COALESCE(client_user_agent, '') <> '' AND COALESCE(internal_reason, '') = ''
		      AND COALESCE(is_retry_attempt, FALSE) = FALSE
		GROUP BY client_user_agent`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	samples := []UsageClientUserAgentSample{}
	for rows.Next() {
		var sample UsageClientUserAgentSample
		var lastSeen any
		if err := rows.Scan(&sample.UserAgent, &sample.Count, &lastSeen); err != nil {
			return nil, err
		}
		sample.LastSeen, err = parseDBTimeValue(lastSeen)
		if err != nil {
			return nil, err
		}
		samples = append(samples, sample)
	}
	return samples, rows.Err()
}
