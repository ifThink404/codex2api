package admin

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestClearUsageLogsInvalidatesOld429Refresh(t *testing.T) {
	db := newTestAdminDB(t)
	h := &Handler{db: db}
	const channel = database.UpstreamChannelCodex
	oldGeneration := h.reqCountGeneration.Load()
	oldCounts := map[int64]*database.AccountRequestCount{42: {AccountID: 42, RateLimitAttemptCount: 2}}
	h.storeRequestCountCache(channel, oldCounts, nil, time.Time{}, oldGeneration)
	staging := h.takeRequestCountStaging(channel, database.StartOfDay(time.Now()))
	staging.counts = oldCounts
	h.saveRequestCountStaging(channel, staging)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("DELETE", "/api/admin/usage/logs", nil)
	h.ClearUsageLogs(c)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Empty(t, h.reqCountCache)
	require.Empty(t, h.reqCountStaging)
	require.Greater(t, h.reqCountGeneration.Load(), oldGeneration)
	// A pre-clear refresh finishes late; neither completed nor partial data may return.
	h.storeRequestCountCache(channel, oldCounts, nil, time.Time{}, oldGeneration)
	h.saveRequestCountStaging(channel, staging)
	require.Empty(t, h.reqCountCache)
	require.Empty(t, h.reqCountStaging)
	newCounts := map[int64]*database.AccountRequestCount{42: {AccountID: 42, RateLimitAttemptCount: 1}}
	h.storeRequestCountCache(channel, newCounts, nil, time.Time{}, h.reqCountGeneration.Load())
	require.Equal(t, int64(1), h.reqCountCache[channel].counts[42].RateLimitAttemptCount)
}
