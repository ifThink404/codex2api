package proxy

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestBPSTimingSurvivesUsageDiagnosticSnapshot(t *testing.T) {
	start := time.Unix(100, 0)
	timing := &bpsTimingDiagnostic{started: start}
	timing.update(func(v *bpsTimingValues) {
		v.ImagePrepareMS, v.FilePrepareMS, v.ToolBridgeMS = 12000, 300, 40
		v.CacheHits, v.CacheMisses, v.CacheWaits = 1, 67, 2
	})
	timing.uploaded(10*time.Second, 4096, 200, false)
	timing.startInference(start.Add(13 * time.Second))
	timing.receivedHeaders(start.Add(15 * time.Second))
	timing.event(start.Add(20*time.Second), true)
	transport := &UpstreamTransportDiagnostic{Transport: "http", BPS: &CodexBPSDiagnostic{Profile: auth.BPSExcel, Mode: "bps", Timing: timing}}
	var decoded UpstreamTransportDiagnostic
	require.NoError(t, json.Unmarshal([]byte(transportDiagnosticJSON(transport)), &decoded))
	usage := &database.UsageLogInput{StatusCode: 200, UpstreamDiagnostics: transportDiagnosticJSON(transport)}
	populateUsageRequestDiagnostics(promptSessionLimitTestContext(""), usage)
	want, err := json.Marshal(timing)
	require.NoError(t, err)
	require.JSONEq(t, string(want), gjson.Get(usage.RequestDiagnostics, "upstream.bps_compat.timing").Raw)
	// A second deserialize/serialize is used by diagnostic readers as well.
	snapshot := readUsageDiagnosticSnapshot(t, usage)
	got, err := json.Marshal(snapshot.Upstream.BPS.Timing)
	require.NoError(t, err)
	require.JSONEq(t, string(want), string(got))
}

func TestBPSTimingSeparatesPreparationHeadersAndContent(t *testing.T) {
	start := time.Unix(100, 0)
	timing := &bpsTimingDiagnostic{started: start}
	timing.uploaded(40*time.Second, 1024, 200, false)
	timing.startInference(start.Add(45 * time.Second))
	timing.receivedHeaders(start.Add(47 * time.Second))
	timing.event(start.Add(48*time.Second), false)
	timing.event(start.Add(55*time.Second), true)
	timing.event(start.Add(65*time.Second), true)
	encoded, err := json.Marshal(timing)
	require.NoError(t, err)
	for field, expected := range map[string]int64{
		"pre_inference_ms": 45000, "upload_ms": 40000, "upload_max_ms": 40000,
		"upload_requests": 1, "upload_bytes": 1024,
		"last_inference_headers_ms": 2000, "last_inference_first_event_ms": 3000,
		"last_inference_first_content_ms": 10000,
	} {
		require.Equal(t, expected, gjson.GetBytes(encoded, field).Int(), field)
	}
	// The retry retains aggregate preparation/upload counts but resets the
	// last inference response markers. An absent marker is not a zero delay.
	timing.startInference(start.Add(70 * time.Second))
	timing.receivedHeaders(start.Add(74 * time.Second))
	encoded, err = json.Marshal(timing)
	require.NoError(t, err)
	require.EqualValues(t, 45000, gjson.GetBytes(encoded, "pre_inference_ms").Int())
	require.EqualValues(t, 6000, gjson.GetBytes(encoded, "inference_headers_total_ms").Int())
	require.EqualValues(t, 4000, gjson.GetBytes(encoded, "last_inference_headers_ms").Int())
	require.False(t, gjson.GetBytes(encoded, "last_inference_first_event_ms").Exists())
	require.False(t, gjson.GetBytes(encoded, "last_inference_first_content_ms").Exists())
}

func TestBPSCacheTimingDistinguishesReadyAndInflight(t *testing.T) {
	timing := &bpsTimingDiagnostic{}
	ctx := context.WithValue(t.Context(), bpsTimingContextKey{}, timing)
	cache := newBPSLocalCache()
	_, _, err := cache.resolve(t.Context(), "ready", func() (string, error) { return "file-existing", nil })
	require.NoError(t, err)
	_, reused, err := cache.resolve(ctx, "ready", func() (string, error) { return "file-unexpected", nil })
	require.NoError(t, err)
	require.True(t, reused)
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		_, _, err := cache.resolve(t.Context(), "pending", func() (string, error) { close(started); <-release; return "file-pending", nil })
		done <- err
	}()
	<-started
	canceled, cancel := context.WithCancel(ctx)
	follower := make(chan error, 1)
	go func() {
		_, _, err := cache.resolve(canceled, "pending", func() (string, error) { return "file-unexpected", nil })
		follower <- err
	}()
	require.Eventually(t, func() bool { cache.mu.Lock(); defer cache.mu.Unlock(); return cache.flights["pending"].waiters == 2 }, time.Second, time.Millisecond)
	cancel()
	require.ErrorIs(t, <-follower, context.Canceled)
	close(release)
	require.NoError(t, <-done)
	_, reused, err = cache.resolve(ctx, "new", func() (string, error) { return "file-new", nil })
	require.NoError(t, err)
	require.False(t, reused)
	encoded, err := json.Marshal(timing)
	require.NoError(t, err)
	require.EqualValues(t, 1, gjson.GetBytes(encoded, "cache_hits").Int())
	require.EqualValues(t, 1, gjson.GetBytes(encoded, "cache_waits").Int())
	require.EqualValues(t, 1, gjson.GetBytes(encoded, "cache_misses").Int())
	require.NotContains(t, string(encoded), "file-existing")
}

func TestBPSCacheTimingExplainsCapacityChurn(t *testing.T) {
	t.Setenv("CODEX_BPS_ATTACHMENT_CACHE_ENTRIES", "2")
	timing := &bpsTimingDiagnostic{}
	ctx := context.WithValue(t.Context(), bpsTimingContextKey{}, timing)
	cache := newBPSLocalCache()
	upload := func() (string, error) { return "file-new", nil }
	for _, key := range []string{"one", "two"} {
		_, _, err := cache.resolve(t.Context(), key, upload)
		require.NoError(t, err)
	}
	_, _, err := cache.resolve(ctx, "new", upload)
	require.NoError(t, err)
	cache.entries["new"].expires = time.Now().Add(-time.Second)
	_, _, err = cache.resolve(ctx, "new", upload)
	require.NoError(t, err)
	encoded, err := json.Marshal(timing)
	require.NoError(t, err)
	for _, key := range []string{"cache_evictions", "cache_expired_entries"} {
		require.EqualValues(t, 1, gjson.GetBytes(encoded, key).Int())
	}
	require.EqualValues(t, 2, gjson.GetBytes(encoded, "cache_entries_at_miss_max").Int())
	require.Zero(t, gjson.GetBytes(encoded, "cache_capacity_bypasses").Int())
	require.Len(t, cache.entries, 2)
}

func TestBPSTimingObserverSkipsEmptyDeltaAndLifecycleContent(t *testing.T) {
	ctx := ensureTransportTrace(t.Context())
	timing := &bpsTimingDiagnostic{started: time.Now()}
	timing.startInference(time.Now())
	audit := upstreamTraceFromContext(ctx)
	audit.current = &upstreamTraceAttempt{transport: UpstreamTransportDiagnostic{BPS: &CodexBPSDiagnostic{Timing: timing}}}
	observer := UpstreamTransportObserver(ctx)
	observer.Event([]byte(`{"type":"response.created"}`))
	observer.Event([]byte(`{"type":"response.output_text.delta","delta":""}`))
	encoded, err := json.Marshal(timing)
	require.NoError(t, err)
	require.True(t, gjson.GetBytes(encoded, "last_inference_first_event_ms").Exists())
	require.False(t, gjson.GetBytes(encoded, "last_inference_first_content_ms").Exists())
	observer.Event([]byte(`{"type":"response.output_text.delta","delta":"hello"}`))
	encoded, err = json.Marshal(timing)
	require.NoError(t, err)
	require.True(t, gjson.GetBytes(encoded, "last_inference_first_content_ms").Exists())
}
