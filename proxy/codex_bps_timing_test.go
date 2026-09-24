package proxy

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

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
	ready := make(chan struct{})
	close(ready)
	cache := &bpsAttachmentCache{entries: map[string]*bpsAttachmentEntry{
		"ready":   {ready: ready, id: "file-existing", expires: time.Now().Add(time.Hour)},
		"pending": {ready: make(chan struct{})},
	}}
	upload := func() (string, error) { t.Fatal("should reuse existing entry"); return "", nil }
	_, reused, err := cache.resolve(ctx, "ready", upload)
	require.NoError(t, err)
	require.True(t, reused)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, reused, err = cache.resolve(canceled, "pending", upload)
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, reused)
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
