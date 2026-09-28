package proxy

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// Only durations, counters and status codes are retained. The collector is
// request-scoped; snapshots may be serialized while a transport is still active.
type bpsTimingValues struct {
	AttachmentDecodes           int                          `json:"attachment_decodes"`
	AttachmentDeduplicated      int                          `json:"attachment_deduplicated"`
	AttachmentHashes            int                          `json:"attachment_hashes"`
	PreparationReuses           int                          `json:"preparation_reuses"`
	UploadQueueMS               int64                        `json:"upload_queue_ms"`
	BufferWaitMS                int64                        `json:"buffer_wait_ms"`
	InstanceUploadLimit         int                          `json:"instance_upload_limit,omitempty"`
	AccountUploadLimit          int                          `json:"account_upload_limit,omitempty"`
	FirstTokenModeAtStart       string                       `json:"first_token_mode_at_start,omitempty"`
	LastInferenceHTTP           *bpsHTTPPhases               `json:"last_inference_http,omitempty"`
	SlowestUploadHTTP           *bpsHTTPPhases               `json:"slowest_upload_http,omitempty"`
	UploadHTTPObservations      int                          `json:"upload_http_observations"`
	PreInferenceMS              *int64                       `json:"pre_inference_ms,omitempty"`
	ImagePrepareMS              int64                        `json:"image_prepare_ms"`
	FilePrepareMS               int64                        `json:"file_prepare_ms"`
	ToolBridgeMS                int64                        `json:"tool_bridge_ms"`
	UploadRequests              int                          `json:"upload_requests"`
	UploadFailures              int                          `json:"upload_failures"`
	UploadBytes                 int64                        `json:"upload_bytes"`
	UploadMS                    int64                        `json:"upload_ms"`
	UploadMaxMS                 int64                        `json:"upload_max_ms"`
	UploadLastHTTPStatus        int                          `json:"upload_last_http_status,omitempty"`
	UploadCanceled              int                          `json:"upload_canceled,omitempty"`
	UploadErrors                []bpsUploadFailureDiagnostic `json:"upload_errors,omitempty"`
	UploadErrorsOmitted         int                          `json:"upload_errors_omitted,omitempty"`
	UploadPrimaryError          *bpsUploadFailureDiagnostic  `json:"upload_primary_error,omitempty"`
	CacheHits                   int                          `json:"cache_hits"`
	SharedCacheHits             int                          `json:"shared_cache_hits"`
	SharedCacheWaits            int                          `json:"shared_cache_waits"`
	SharedCacheWaitMS           int64                        `json:"shared_cache_wait_ms"`
	SharedCacheErrors           int                          `json:"shared_cache_errors"`
	CacheMisses                 int                          `json:"cache_misses"`
	CacheWaits                  int                          `json:"cache_waits"`
	CacheWaitMS                 int64                        `json:"cache_wait_ms"`
	CacheExpiredEntries         int                          `json:"cache_expired_entries"`
	CacheEvictions              int                          `json:"cache_evictions"`
	CacheCapacityBypasses       int                          `json:"cache_capacity_bypasses"`
	CacheEntriesAtMissMax       int                          `json:"cache_entries_at_miss_max"`
	CacheEntryLimit             int                          `json:"cache_entry_limit,omitempty"`
	UploadConcurrencyLimit      int                          `json:"upload_concurrency_limit,omitempty"`
	AttachmentRetries           int                          `json:"attachment_retries"`
	InferenceAttempts           int                          `json:"inference_attempts"`
	InferenceHeadersTotalMS     int64                        `json:"inference_headers_total_ms"`
	LastInferenceHeadersMS      *int64                       `json:"last_inference_headers_ms,omitempty"`
	LastInferenceFirstEventMS   *int64                       `json:"last_inference_first_event_ms,omitempty"`
	LastInferenceFirstContentMS *int64                       `json:"last_inference_first_content_ms,omitempty"`
}

type bpsTimingDiagnostic struct {
	mu             sync.Mutex
	values         bpsTimingValues
	started        time.Time
	inferenceStart time.Time
}

type bpsTimingContextKey struct{}

func bpsTimingFromContext(ctx context.Context) *bpsTimingDiagnostic {
	t, _ := ctx.Value(bpsTimingContextKey{}).(*bpsTimingDiagnostic)
	return t
}

func (t *bpsTimingDiagnostic) update(fn func(*bpsTimingValues)) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	fn(&t.values)
}

func (t *bpsTimingDiagnostic) MarshalJSON() ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return json.Marshal(t.values)
}

// Usage diagnostics decode the transport snapshot before storing it. The
// collector's private fields must round-trip rather than silently becoming zero.
func (t *bpsTimingDiagnostic) UnmarshalJSON(data []byte) error {
	var values bpsTimingValues
	if err := json.Unmarshal(data, &values); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.values = values
	t.started, t.inferenceStart = time.Time{}, time.Time{}
	return nil
}

func (t *bpsTimingDiagnostic) startInference(now time.Time) {
	t.update(func(v *bpsTimingValues) {
		if v.InferenceAttempts == 0 {
			ms := max(now.Sub(t.started).Milliseconds(), 0)
			v.PreInferenceMS = &ms
		}
		v.InferenceAttempts++
		t.inferenceStart = now
		v.LastInferenceHTTP = nil
		v.LastInferenceHeadersMS, v.LastInferenceFirstEventMS, v.LastInferenceFirstContentMS = nil, nil, nil
	})
}

func (t *bpsTimingDiagnostic) receivedHeaders(now time.Time) {
	t.update(func(v *bpsTimingValues) {
		if t.inferenceStart.IsZero() {
			return
		}
		ms := max(now.Sub(t.inferenceStart).Milliseconds(), 0)
		v.LastInferenceHeadersMS = &ms
		v.InferenceHeadersTotalMS += ms
	})
}

func (t *bpsTimingDiagnostic) event(now time.Time, content bool) {
	t.update(func(v *bpsTimingValues) {
		if t.inferenceStart.IsZero() {
			return
		}
		ms := max(now.Sub(t.inferenceStart).Milliseconds(), 0)
		if v.LastInferenceFirstEventMS == nil {
			v.LastInferenceFirstEventMS = &ms
		}
		if content && v.LastInferenceFirstContentMS == nil {
			v.LastInferenceFirstContentMS = &ms
		}
	})
}

func (t *bpsTimingDiagnostic) uploaded(elapsed time.Duration, bytes int, status int, failed bool) {
	t.update(func(v *bpsTimingValues) {
		ms := max(elapsed.Milliseconds(), 0)
		v.UploadRequests++
		v.UploadBytes += int64(bytes)
		v.UploadMS += ms
		v.UploadMaxMS = max(v.UploadMaxMS, ms)
		// Sibling cancellation has no HTTP response and must not erase the
		// real status that caused a parallel upload group to stop.
		if status != 0 {
			v.UploadLastHTTPStatus = status
		}
		if failed {
			v.UploadFailures++
		}
	})
}

func (t *bpsTimingDiagnostic) uploadFailed(d bpsUploadFailureDiagnostic) {
	t.update(func(v *bpsTimingValues) {
		if d.Stage == "canceled" {
			v.UploadCanceled++
		}
		if v.UploadPrimaryError == nil || v.UploadPrimaryError.Stage == "canceled" && d.Stage != "canceled" {
			copy := d
			v.UploadPrimaryError = &copy
		}
		if len(v.UploadErrors) < 8 {
			v.UploadErrors = append(v.UploadErrors, d)
		} else {
			v.UploadErrorsOmitted++
		}
	})
}

func (t *bpsTimingDiagnostic) uploadFailure() *bpsUploadFailureDiagnostic {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.values.UploadPrimaryError == nil {
		return nil
	}
	d := *t.values.UploadPrimaryError
	return &d
}
