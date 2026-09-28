package proxy

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func earlySSEResponsesHandler(t *testing.T, route, upstreamURL string) (*Handler, string) {
	t.Helper()
	if route == "relay" {
		store := newOpenAIResponsesRelayStore(upstreamURL)
		store.SetMaxRetries(1)
		t.Cleanup(store.Stop)
		return NewHandler(store, nil, nil, nil), "gpt-4.1-direct"
	}
	previous := GetResinConfig()
	t.Cleanup(func() { SetResinConfig(previous) })
	SetResinConfig(&ResinConfig{BaseURL: upstreamURL, PlatformName: "early-sse-test"})
	t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 1})
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "test-token", PlanType: "pro", AccountID: "test-account"})
	t.Cleanup(store.Stop)
	return NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil), "gpt-5.5"
}

func TestEarlySSEPassthroughHTTPDelivery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, route := range []string{"codex", "relay"} {
		for _, tc := range []struct {
			name                           string
			early, report, retry, catchAll bool
			coalesce, nonstream, wantEarly bool
		}{
			{name: "default_buffered"},
			{name: "timing_report_only", report: true},
			{name: "created_before_content", early: true, wantEarly: true},
			{name: "created_with_coalescing", early: true, report: true, coalesce: true, wantEarly: true},
			{name: "selective_retry_buffered", early: true, retry: true},
			{name: "catch_all_retry_buffered", early: true, retry: true, catchAll: true},
			{name: "nonstream_unchanged", early: true, nonstream: true},
		} {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				previous := CurrentRuntimeSettings()
				t.Cleanup(func() { ApplyRuntimeSettings(previous) })
				settings := DefaultRuntimeSettings()
				settings.CodexForceWebsocket = false
				settings.FirstTokenMode = FirstTokenModeStrict
				settings.CodexEarlySSEPassthrough = tc.early
				settings.CodexPreflightSSEPassthrough = tc.report
				settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{Enabled: tc.retry, CatchAll: tc.catchAll}
				if tc.coalesce {
					settings.StreamFlushPolicy = StreamFlushPolicyCoalesce
					settings.StreamFlushIntervalMS = 1000
				}
				ApplyRuntimeSettings(settings)

				release := make(chan struct{})
				var releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				defer unblock()
				preludeSent := make(chan struct{}, 1)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if tc.nonstream && route == "relay" {
						// A Responses API relay honors stream:false with JSON;
						// the native Codex upstream always generates SSE.
						preludeSent <- struct{}{}
						select {
						case <-release:
						case <-r.Context().Done():
							return
						}
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"id":"early-response","status":"completed","output":[],"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":2}}}`)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"type\":\"codex.rate_limits\"}\n\n"+
						"data: {\"type\":\"response.created\",\"response\":{\"id\":\"early-response\",\"status\":\"in_progress\"}}\n\n"+
						"data: {\"type\":\"response.in_progress\",\"response\":{\"id\":\"early-response\"}}\n\n")
					w.(http.Flusher).Flush()
					preludeSent <- struct{}{}
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
					_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"+
						"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"early-response\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":11,\"output_tokens\":7,\"total_tokens\":18,\"input_tokens_details\":{\"cached_tokens\":3},\"output_tokens_details\":{\"reasoning_tokens\":2}}}}\n\n")
				}))
				t.Cleanup(upstream.Close)
				h, model := earlySSEResponsesHandler(t, route, upstream.URL)
				downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					c, _ := gin.CreateTestContext(w)
					c.Request = r
					h.Responses(c)
				}))
				t.Cleanup(downstream.Close)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				request, err := http.NewRequestWithContext(ctx, http.MethodPost, downstream.URL+"/v1/responses",
					strings.NewReader(fmt.Sprintf(`{"model":%q,"input":"hello","stream":%t}`, model, !tc.nonstream)))
				require.NoError(t, err)
				request.Header.Set("Content-Type", "application/json")
				events := make(chan string, 16)
				type result struct {
					body   string
					status int
					err    error
				}
				done := make(chan result, 1)
				go func() {
					resp, err := downstream.Client().Do(request)
					if err != nil {
						done <- result{err: err}
						return
					}
					defer resp.Body.Close()
					var body strings.Builder
					scanner := bufio.NewScanner(resp.Body)
					for scanner.Scan() {
						line := scanner.Text()
						body.WriteString(line + "\n")
						if strings.HasPrefix(line, "data: ") {
							events <- strings.TrimPrefix(line, "data: ")
						}
					}
					done <- result{body: body.String(), status: resp.StatusCode, err: scanner.Err()}
				}()
				select {
				case <-preludeSent:
				case <-ctx.Done():
					t.Fatal("upstream did not receive the request")
				}
				if tc.wantEarly {
					for _, want := range []string{"codex.rate_limits", "response.created", "response.in_progress"} {
						select {
						case event := <-events:
							require.Equal(t, want, gjson.Get(event, "type").String(), event)
						case <-ctx.Done():
							t.Fatalf("%s stayed buffered while upstream waited to generate content", want)
						}
					}
				} else {
					select {
					case event := <-events:
						t.Fatalf("event exposed before release: %s", event)
					case r := <-done:
						t.Fatalf("response completed before release: %+v", r)
					case <-time.After(100 * time.Millisecond):
					}
				}
				unblock()
				var got result
				select {
				case got = <-done:
				case <-ctx.Done():
					t.Fatal("response did not finish after release")
				}
				require.NoError(t, got.err)
				require.Equal(t, http.StatusOK, got.status, got.body)
				var usage gjson.Result
				if tc.nonstream {
					require.True(t, gjson.Valid(got.body), got.body)
					usage = gjson.Get(got.body, "usage")
				} else {
					require.Equal(t, 1, strings.Count(got.body, `"type":"response.created"`), got.body)
					require.Equal(t, 1, strings.Count(got.body, `"type":"response.completed"`), got.body)
					for _, line := range strings.Split(got.body, "\n") {
						event := gjson.Parse(strings.TrimPrefix(line, "data: "))
						if event.Get("type").String() == "response.completed" {
							usage = event.Get("response.usage")
						}
					}
				}
				require.EqualValues(t, 11, usage.Get("input_tokens").Int())
				require.EqualValues(t, 7, usage.Get("output_tokens").Int())
				require.EqualValues(t, 3, usage.Get("input_tokens_details.cached_tokens").Int())
				require.EqualValues(t, 2, usage.Get("output_tokens_details.reasoning_tokens").Int())
			})
		}
	}
}

func TestEarlySSEPassthroughFailureBoundary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, route := range []string{"codex", "relay"} {
		for _, mode := range []string{"buffered", "early", "continuous_retry"} {
			t.Run(route+"/"+mode, func(t *testing.T) {
				previous := CurrentRuntimeSettings()
				t.Cleanup(func() { ApplyRuntimeSettings(previous) })
				settings := DefaultRuntimeSettings()
				settings.CodexForceWebsocket = false
				settings.CodexEarlySSEPassthrough = mode != "buffered"
				settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{Enabled: mode == "continuous_retry", CatchAll: true}
				ApplyRuntimeSettings(settings)
				upstream, calls := newAttemptSequenceSSEServer(t, [][]string{
					{`{"type":"response.created","response":{"id":"failed-attempt"}}`, `{"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","message":"temporary upstream failure"}}}`},
					{`{"type":"response.created","response":{"id":"winning-attempt"}}`, `{"type":"response.output_text.delta","delta":"recovered"}`, `{"type":"response.completed","response":{"id":"winning-attempt","status":"completed","usage":{"input_tokens":11,"output_tokens":7}}}`},
				})
				h, model := earlySSEResponsesHandler(t, route, upstream.URL)
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(fmt.Sprintf(`{"model":%q,"input":"hello","stream":true}`, model)))
				h.Responses(c)
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				if mode == "early" {
					require.EqualValues(t, 1, calls.Load(), "must not replay a response already visible to the client")
					require.Equal(t, 1, strings.Count(recorder.Body.String(), `"type":"response.created"`))
					require.Contains(t, recorder.Body.String(), `"type":"response.failed"`)
					require.NotContains(t, recorder.Body.String(), "winning-attempt")
				} else {
					require.EqualValues(t, 2, calls.Load(), recorder.Body.String())
					require.Contains(t, recorder.Body.String(), "recovered")
					require.NotContains(t, recorder.Body.String(), "failed-attempt")
					require.NotContains(t, recorder.Body.String(), `"type":"response.failed"`)
				}
			})
		}
	}
}
