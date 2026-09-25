package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const rawCompletedEvent = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n\n"

type rawCancelAfterWrite struct {
	gin.ResponseWriter
	cancel    context.CancelFunc
	remaining int
}

func (w *rawCancelAfterWrite) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.remaining -= n
	if w.remaining <= 0 {
		w.cancel()
	}
	return n, err
}

func TestRawRelayStreamEndDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, payload, ending, outcome, responseError, terminal string
	}{
		{"completed_eof", rawCompletedEvent, "eof", "completed", "", "response.completed"},
		{"completed_then_abrupt_close", rawCompletedEvent, "read_error", "completed", "", "response.completed"},
		{"completed_then_client_cancel", rawCompletedEvent, "cancel", "completed", "", "response.completed"},
		{"partial_then_client_cancel", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n", "cancel", "interrupted", "downstream_canceled", ""},
		{"usage_is_not_completion", "data: {\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n\n", "read_error", "interrupted", "upstream_read_failed", ""},
		{"failed_then_done", "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"message\":\"provider failed\"}}}\n\ndata: [DONE]\n\n", "eof", "failed", "upstream_response_failed", "response.failed"},
		{"incomplete_then_close", "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n", "read_error", "incomplete", "upstream_response_incomplete", "response.incomplete"},
		{"unterminated_event", rawCompletedEvent[:len(rawCompletedEvent)-1], "read_error", "interrupted", "upstream_read_failed", ""},
		{"vendor_eof_without_known_terminal", "data: {\"vendor\":\"untouched\"}\n\n", "eof", "unknown", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var calls atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				if tc.ending == "read_error" {
					// A short Content-Length produces a genuine unexpected EOF.
					w.Header().Set("Content-Length", fmt.Sprint(len(tc.payload)+1))
				}
				_, _ = io.WriteString(w, tc.payload)
				w.(http.Flusher).Flush()
				if tc.ending == "cancel" {
					<-r.Context().Done()
				}
			}))
			defer up.Close()
			h, row, _ := newModelQuotaTestHandler(t, 100, up.URL, false)
			addRawRelayTestAccount(h, up.URL)
			h.db.SetUsageLogConfig(database.UsageLogModeFull, 100, 60)
			c, recorder := rawRoutingTestContext(row, "/v1/responses", []byte(`{"model":"gpt-6-astra","stream":true}`), nil)
			c.Request = c.Request.WithContext(ctx)
			if tc.ending == "cancel" {
				c.Writer = &rawCancelAfterWrite{ResponseWriter: c.Writer, cancel: cancel, remaining: len(tc.payload)}
			}
			require.True(t, h.tryRawRelay(c))
			require.Equal(t, http.StatusOK, recorder.Code)
			require.Equal(t, tc.payload, recorder.Body.String(), "diagnosis must never alter the raw response")
			require.EqualValues(t, 1, calls.Load(), "no replay after streaming starts")
			h.db.FlushUsageLogs()
			var logID int64
			var statusCode int
			var errorMessage string
			require.Eventually(t, func() bool {
				logs, err := h.db.ListUsageLogsByTimeRange(t.Context(), time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
				if err != nil || len(logs) != 1 {
					return false
				}
				errorMessage, statusCode = logs[0].ErrorMessage, logs[0].StatusCode
				logID = logs[0].ID
				return true
			}, 2*time.Second, 10*time.Millisecond)
			require.Equal(t, tc.responseError, errorMessage)
			require.Equal(t, http.StatusOK, statusCode)
			detail, err := h.db.GetUsageRequestDiagnostics(t.Context(), logID)
			require.NoError(t, err)
			d := gjson.GetBytes(detail.Diagnostics, "raw_passthrough")
			require.Equal(t, tc.outcome, d.Get("stream_outcome").String())
			require.Equal(t, tc.terminal, d.Get("terminal_event").String())
			require.Equal(t, tc.responseError, d.Get("response_error").String())
			if tc.ending == "read_error" {
				require.Equal(t, "unexpected EOF", d.Get("read_error.message").String())
				require.Equal(t, "unexpected_eof", d.Get("read_error.kind").String())
			}
			if tc.ending == "cancel" {
				require.True(t, d.Get("client_canceled").Bool())
				require.Equal(t, "context canceled", d.Get("request_context_error").String())
			}
			if tc.outcome == "incomplete" {
				require.Equal(t, "max_output_tokens", d.Get("incomplete_reason").String())
			}
			if tc.outcome == "failed" {
				require.Equal(t, "provider failed", d.Get("terminal_error").String())
			}
		})
	}
}

func TestRawRelayRealClientClosesAfterCompleted(t *testing.T) {
	upstreamCanceled := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, rawCompletedEvent)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	defer up.Close()
	h, _, router := newModelQuotaTestHandler(t, 100, up.URL, false)
	addRawRelayTestAccount(h, up.URL)
	h.db.SetUsageLogConfig(database.UsageLogModeFull, 100, 60)
	server := httptest.NewServer(router)
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-6-astra","stream":true}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+modelQuotaTestKey)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	wire := make([]byte, len(rawCompletedEvent))
	_, err = io.ReadFull(resp.Body, wire)
	require.NoError(t, err)
	require.Equal(t, rawCompletedEvent, string(wire))
	cancel()
	require.NoError(t, resp.Body.Close())
	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("client cancellation did not propagate upstream")
	}
	var diagnostic []byte
	var errorMessage string
	require.Eventually(t, func() bool {
		h.db.FlushUsageLogs()
		logs, err := h.db.ListUsageLogsByTimeRange(t.Context(), time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
		if err != nil || len(logs) != 1 {
			return false
		}
		errorMessage = logs[0].ErrorMessage
		detail, err := h.db.GetUsageRequestDiagnostics(t.Context(), logs[0].ID)
		if err != nil || detail == nil {
			return false
		}
		diagnostic = detail.Diagnostics
		return true
	}, 2*time.Second, 10*time.Millisecond)
	require.Empty(t, errorMessage)
	require.Equal(t, "completed", gjson.GetBytes(diagnostic, "raw_passthrough.stream_outcome").String())
	require.True(t, gjson.GetBytes(diagnostic, "raw_passthrough.client_canceled").Bool())
	require.Equal(t, "context_canceled", gjson.GetBytes(diagnostic, "raw_passthrough.read_error.kind").String())
}

func TestRawRelayTerminalObservationBoundaries(t *testing.T) {
	for _, tc := range []struct{ name, payload, event, status string }{
		{"fragmented", rawCompletedEvent, "response.completed", "completed"},
		{"crlf", strings.ReplaceAll(rawCompletedEvent, "\n", "\r\n"), "response.completed", "completed"},
		{"multiline", "event: response.completed\ndata: {\ndata: \"response\": {\"status\":\"completed\"}}\n\n", "response.completed", "completed"},
		{"chat_done", "data: [DONE]\n\n", "[DONE]", "completed"},
		{"messages_stop", "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", "message_stop", "completed"},
		{"responses_done_is_not_completion", "data: {\"type\":\"response.created\"}\n\ndata: [DONE]\n\n", "", ""},
		{"legacy_missing_status", "data: {\"type\":\"response.done\"}\n\n", "response.done", "unknown"},
		{"contradicting_status", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"incomplete\"}}\n\n", "response.completed", "incomplete"},
		{"failure_then_stop", "data: {\"type\":\"error\",\"error\":{\"message\":\"failed\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n", "error", "failed"},
		{"chat_error_then_done", "data: {\"error\":{\"message\":\"failed\"}}\n\ndata: [DONE]\n\n", "error", "failed"},
		{"malformed_json", "data: {\"type\":\"response.completed\"\n\n", "", ""},
		{"unterminated_line", strings.TrimRight(rawCompletedEvent, "\n"), "", ""},
		{"unterminated_event", rawCompletedEvent[:len(rawCompletedEvent)-1], "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := &rawRelayUsageObserver{stream: true}
			for _, b := range []byte(tc.payload) {
				o.Write([]byte{b})
			}
			o.finish()
			require.Equal(t, tc.event, o.terminalEvent)
			require.Equal(t, tc.status, o.responseStatus)
		})
	}
	var compressed bytes.Buffer
	z := gzip.NewWriter(&compressed)
	_, err := z.Write([]byte(rawCompletedEvent))
	require.NoError(t, err)
	require.NoError(t, z.Close())
	o := &rawRelayUsageObserver{stream: true, encoding: "gzip"}
	o.Write(compressed.Bytes())
	o.finish()
	require.Equal(t, "response.completed", o.terminalEvent)
	require.Equal(t, "completed", o.responseStatus)
	require.Equal(t, 10, o.usage.InputTokens)

	o = &rawRelayUsageObserver{stream: true}
	o.Write([]byte("data: "))
	o.Write(bytes.Repeat([]byte("x"), 4<<20))
	o.Write([]byte("\n" + rawCompletedEvent)) // Same oversized event: cannot trust the suffix.
	require.Empty(t, o.terminalEvent)
	o.Write([]byte(rawCompletedEvent)) // The next complete event remains observable.
	o.finish()
	require.Equal(t, "completed", o.responseStatus)
}

func TestRawRelayWriteFailureAndDeadlineNeverBecomeSuccess(t *testing.T) {
	o := &rawRelayUsageObserver{stream: true}
	o.Write([]byte(rawCompletedEvent))
	d := &rawRelayDiagnostic{}
	d.finishStream(nil, o, io.EOF, io.ErrShortWrite, nil)
	require.Equal(t, "downstream_write_failed", d.ResponseError)
	require.Equal(t, "interrupted", d.StreamOutcome)
	require.Equal(t, "completed", d.ResponseStatus)
	require.Equal(t, "short_write", d.WriteError.Kind)

	d = &rawRelayDiagnostic{}
	d.finishStream(nil, &rawRelayUsageObserver{stream: true}, context.DeadlineExceeded, nil, context.DeadlineExceeded)
	require.Equal(t, "request_deadline_exceeded", d.ResponseError)
	require.False(t, d.ClientCanceled)
	require.Equal(t, "deadline_exceeded", d.ReadError.Kind)

	d = &rawRelayDiagnostic{}
	d.finishStream(nil, &rawRelayUsageObserver{}, io.EOF, nil, context.Canceled)
	require.Empty(t, d.ResponseError, "cancellation racing with a clean EOF must not invent a read failure")
	require.True(t, d.ClientCanceled)

	o = &rawRelayUsageObserver{stream: true}
	o.Write([]byte("data: {\"type\":\"error\",\"message\":\"provider failure detail\"}\n\n"))
	d = &rawRelayDiagnostic{}
	d.finishStream(nil, o, io.EOF, nil, nil)
	require.Equal(t, "provider failure detail", d.TerminalError)

	detail := rawRelayErrorDetail(nil, errors.New("read https://user:secret@private.example/path?token=private: unexpected failure "+strings.Repeat("a", 1500)))
	require.NotContains(t, detail.Message, "secret")
	require.NotContains(t, detail.Message, "private")
	require.LessOrEqual(t, len(detail.Message), 1000)
}
