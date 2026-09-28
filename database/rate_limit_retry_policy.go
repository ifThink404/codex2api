package database

import (
	"context"
	"strings"
)

const (
	RateLimitRetryOff    = "off"
	RateLimitRetrySticky = "sticky"
	RateLimitRetryRotate = "rotate"
)

func NormalizeRateLimitRetryPolicy(policy string) string {
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case RateLimitRetryOff:
		return RateLimitRetryOff
	case RateLimitRetrySticky:
		return RateLimitRetrySticky
	default:
		return RateLimitRetryRotate
	}
}

// Empty means an old settings writer: take its legacy policy once, then store
// an explicit value so later transport-policy changes cannot change 429s.
func ResolveRateLimitRetryPolicy(policy, legacyTransport string) string {
	if strings.TrimSpace(policy) == "" {
		policy = legacyTransport
	}
	return NormalizeRateLimitRetryPolicy(policy)
}

func (db *DB) backfillRateLimitRetryPolicy(ctx context.Context) error {
	_, err := db.conn.ExecContext(ctx, `UPDATE system_settings SET rate_limit_retry_policy =
		CASE WHEN LOWER(TRIM(transport_retry_policy)) = 'sticky' THEN 'sticky' ELSE 'rotate' END
		WHERE COALESCE(TRIM(rate_limit_retry_policy), '') = ''`)
	return err
}
