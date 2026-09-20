package database

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRecentUsageClientUserAgentsOnlyUsesInboundAndBoundsScan(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "ua.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	start := time.Now().UTC().Truncate(time.Second)
	fixtures := []struct {
		client, upstream, internal string
		retry                      bool
	}{
		{"old-client", "", "", false},
		{"observed-client", "upstream-only", "", false},
		{"observed-client", "upstream-only", "", false},
		{"", "upstream-only", "", false},
		{"retry-client", "", "", true},
		{"internal-client", "", "internal-test", false},
	}
	for i, item := range fixtures {
		_, err = db.conn.ExecContext(ctx, `INSERT INTO usage_logs(account_id, endpoint, model, status_code, created_at, client_user_agent, upstream_user_agent, internal_reason, is_retry_attempt)
			VALUES (0, '/v1/responses', 'test', 200, $1, $2, $3, $4, $5)`, sqliteTimeParam(start.Add(time.Duration(i)*time.Second)), item.client, item.upstream, item.internal, item.retry)
		require.NoError(t, err)
	}
	samples, err := db.recentUsageClientUserAgents(ctx, 5)
	require.NoError(t, err)
	require.Len(t, samples, 1)
	require.Equal(t, "observed-client", samples[0].UserAgent)
	require.EqualValues(t, 2, samples[0].Count)
	require.True(t, start.Add(2*time.Second).Equal(samples[0].LastSeen))
	samples, err = db.RecentUsageClientUserAgents(ctx)
	require.NoError(t, err)
	require.Len(t, samples, 2)
}
