package proxy

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const rawPrivacyResponse = `{"object":"response","id":"resp_keep","previous_response_id":"resp_previous","model":"vendor/custom","metadata":{"task_id":"secret-task","future_internal":"secret-future"},"headers":{"Authorization":"secret-auth","User-Agent":"secret-UA","X-Request-ID":"trace-keep"},"output":[{"type":"function_call","id":"item_keep","call_id":"call_keep","name":"lookup","arguments":"{\"metadata\":{\"task_id\":\"business-task\"}}","metadata":{"turn_id":"secret-turn"}},{"type":"reasoning","encrypted_content":"opaque-keep","summary":[{"text":"metadata business"}]}],"usage":{"input_tokens":9,"output_tokens":2},"vendor_number":9007199254740993}`

var rawPrivacyHeaders = []string{
	"Authorization", "Proxy-Authorization", "Cookie", "Set-Cookie", "User-Agent", "X-User-Agent", "X-Api-Key",
	"Chatgpt-Account-Id", "X-OpenAI-Account-Id", "OpenAI-Organization", "OpenAI-Project",
	"X-OpenAI-Organization", "OAI-Device-Id", "X-Session-Token", "X-Upstream-Secret",
	"X-Codex-Installation-Id", "X-Codex-Window-Id", "X-Device-Id", "Sec-CH-UA",
	"X-OpenAI-Internal-Basispoints-Client-Host", "X-Basispoints-Auth-Mode", "X-NewAPI-Signature", "X-Codex2API-Policy",
}

func TestRawRelayPrivacyHTTPBoundaries(t *testing.T) {
	for _, endpoint := range []string{"/v1/responses", "/v1/responses/compact", "/v1/chat/completions", "/v1/messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", endpoint, stream), func(t *testing.T) {
				payload := rawPrivacyResponse
				if strings.HasSuffix(endpoint, "/compact") {
					payload = strings.Replace(payload, `"object":"response"`, `"object":"response.compaction"`, 1)
				}
				if stream {
					payload = "event: response.completed\r\ndata: {\"type\":\"response.completed\",\"response\":" + payload + "}\r\n\r\ndata: [DONE]\n\n"
				}
				requestBody := ` {"model":"vendor/custom","metadata":{"task_id":"caller-request"},"input":[],"tools":[{"type":"function","name":"lookup","parameters":{"properties":{"account_id":{"type":"string"}}}}]}`
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					require.NoError(t, err)
					require.Equal(t, requestBody, string(body))
					require.Equal(t, "caller-UA", r.Header.Get("User-Agent"))
					require.Equal(t, "caller-device", r.Header.Get("X-Codex-Installation-Id"))
					require.Equal(t, "Bearer upstream-only-key", r.Header.Get("Authorization"))
					for _, name := range rawPrivacyHeaders {
						w.Header().Set(name, "sensitive-header")
					}
					w.Header().Set("Content-Type", "application/json")
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
					}
					w.Header().Set("Trailer", "X-OpenAI-Account-Id, X-Finish, ETag")
					w.Header().Set("X-Request-ID", "trace-keep")
					w.Header().Set("Retry-After", "3")
					w.Header().Set("X-Ratelimit-Remaining-Requests", "12")
					w.Header().Set("OpenAI-Model", "vendor/custom")
					w.Header().Set("ETag", "stale-integrity")
					_, _ = io.WriteString(w, payload)
					w.Header().Set("X-OpenAI-Account-Id", "sensitive-trailer")
					w.Header().Set("X-Finish", "done")
					w.Header().Set("ETag", "stale-trailer-integrity")
				}))
				defer up.Close()
				h, _, router := newModelQuotaTestHandler(t, 100, up.URL, false)
				addRawRelayTestAccount(h, up.URL)
				req := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(requestBody))
				req.Header.Set("Authorization", "Bearer "+modelQuotaTestKey)
				req.Header.Set("User-Agent", "caller-UA")
				req.Header.Set("X-Codex-Installation-Id", "caller-device")
				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)
				require.Equal(t, 200, w.Code, w.Body.String())
				for _, name := range rawPrivacyHeaders {
					require.Empty(t, w.Header().Get(name), name)
					require.Empty(t, w.Result().Trailer.Get(name), name)
				}
				require.Empty(t, w.Header().Get("ETag"))
				require.Empty(t, w.Result().Trailer.Get("ETag"))
				require.Empty(t, w.Header().Get("Content-Length"))
				require.Equal(t, "done", w.Result().Trailer.Get("X-Finish"))
				require.Equal(t, "trace-keep", w.Header().Get("X-Request-ID"))
				require.Equal(t, "3", w.Header().Get("Retry-After"))
				require.Equal(t, "12", w.Header().Get("X-Ratelimit-Remaining-Requests"))
				result := w.Body.String()
				for _, marker := range []string{"secret-task", "secret-future", "secret-auth", "secret-UA", "secret-turn"} {
					require.NotContains(t, result, marker)
				}
				for _, keep := range []string{"resp_keep", "resp_previous", "call_keep", "item_keep", "business-task", "opaque-keep", "9007199254740993", "vendor/custom"} {
					require.Contains(t, result, keep)
				}
				observer := &rawRelayUsageObserver{stream: stream}
				observer.Write(w.Body.Bytes())
				observer.finish()
				require.NotNil(t, observer.usage)
				require.Equal(t, 9, observer.usage.InputTokens)
				if stream {
					require.Equal(t, "response.completed", observer.terminalEvent)
				}
			})
		}
	}
}

func rawPrivacyEncode(t *testing.T, data []byte, encoding string) []byte {
	t.Helper()
	var output bytes.Buffer
	var writer io.WriteCloser
	switch encoding {
	case "gzip":
		writer = gzip.NewWriter(&output)
	case "br":
		writer = brotli.NewWriter(&output)
	case "deflate":
		writer = zlib.NewWriter(&output)
	case "zstd":
		var err error
		writer, err = zstd.NewWriter(&output)
		require.NoError(t, err)
	}
	_, err := writer.Write(data)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return output.Bytes()
}

func TestRawRelayPrivacyCompressionAndErrorBodies(t *testing.T) {
	for _, encoding := range []string{"gzip", "br", "deflate", "zstd"} {
		for _, status := range []int{200, 429} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%d/stream=%v", encoding, status, stream), func(t *testing.T) {
					payload := rawPrivacyResponse
					if status >= 400 {
						payload = `{"error":{"code":"vendor_error","message":"preserve private-account for debugging"},"metadata":{"raw":"keep-error"}}`
					}
					if stream {
						payload = "data: " + payload + "\n\n"
					}
					encoded := rawPrivacyEncode(t, []byte(payload), encoding)
					resp := &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}, "Content-Encoding": {encoding}, "Content-Length": {fmt.Sprint(len(encoded))}}, Body: io.NopCloser(bytes.NewReader(encoded))}
					if stream {
						resp.Header.Set("Content-Type", "text/event-stream")
					}
					require.NoError(t, prepareRawRelayResponsePrivacy(resp))
					defer resp.Body.Close()
					out, err := io.ReadAll(resp.Body)
					require.NoError(t, err)
					if status >= 400 {
						require.Equal(t, encoded, out)
						require.Equal(t, encoding, resp.Header.Get("Content-Encoding"))
					} else {
						require.NotContains(t, string(out), "secret-")
						require.Contains(t, string(out), "business-task")
						require.Empty(t, resp.Header.Get("Content-Encoding"))
					}
				})
			}
		}
	}
}

func TestRawRelayPrivacyBusinessAndMalformedBoundaries(t *testing.T) {
	for _, data := range []string{
		`{"type":"error","message":"raw task_id account","metadata":{"task_id":"preserved-error"}}`,
		`{"type":"response.output_text.delta","delta":"{\"metadata\":{\"account_id\":\"business\"}}"}`,
		`{"choices":[{"message":{"content":"business text","tool_calls":[{"function":{"name":"check","arguments":"{\"account_id\":\"business\"}"}}]}}]}`,
		`{"output":[{"type":"function_call_output","output":{"metadata":{"task_id":"business"}}},{"type":"computer_call","action":{"metadata":{"task_id":"business"}}},{"type":"mcp_call","error":{"account_id":"business"}}]}`,
		`{"tools":[{"parameters":{"properties":{"metadata":{"type":"object"}}}}],"encrypted_content":"opaque","conversation":{"id":"conversation_keep"}}`,
	} {
		out, err := sanitizeRawRelayJSON([]byte(data), false, 0)
		require.NoError(t, err)
		require.Equal(t, data, string(out))
	}
	data := `{"type":"response.failed","response":{"metadata":{"secret":"drop"},"error":{"message":"original failure","metadata":{"account":"keep-error"}}}}`
	out, err := sanitizeRawRelayJSON([]byte(data), false, 0)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(out, "response.metadata").Exists())
	require.Equal(t, gjson.Get(data, "response.error").Raw, gjson.GetBytes(out, "response.error").Raw)
	for _, data := range []string{`{"metadata":`, `{"headers":{"Authorization":"secret"}`, strings.Repeat(`{"nested":`, 70) + `{}` + strings.Repeat(`}`, 70)} {
		_, err := sanitizeRawRelayJSON([]byte(data), false, 0)
		require.Error(t, err)
	}
	data = `{"metadata":{"task_id":"first"},"metadata":{"task_id":"last"},"headers":{"X-Device-ID":"private","X-Request-ID":"keep","Unknown":{"account_id":"private"}},"id":"resp_keep"}`
	out, err = sanitizeRawRelayJSON([]byte(data), false, 0)
	require.NoError(t, err)
	require.NotContains(t, string(out), "private")
	require.False(t, gjson.GetBytes(out, "metadata").Exists())
	require.Equal(t, "keep", gjson.GetBytes(out, "headers.X-Request-ID").String())
	frame := []byte("event: response.created\r\nid: keep-event\r\ndata: {\r\ndata: \"response\": " + rawPrivacyResponse + "}\r\n\r\n")
	out, err = sanitizeRawRelayFrame(frame)
	require.NoError(t, err)
	require.Contains(t, string(out), "id: keep-event\r\n")
	require.NotContains(t, string(out), "secret-task")
	errorFrame := []byte("event: error\ndata: {\"message\":\"raw error\",\"metadata\":{\"task_id\":\"preserve-error\"}}\n\n")
	out, err = sanitizeRawRelayFrame(errorFrame)
	require.NoError(t, err)
	require.Equal(t, errorFrame, out)
	for _, prefix := range []string{"\xef\xbb\xbf", "event: error\nevent: response.created\n"} {
		out, err = sanitizeRawRelayFrame([]byte(prefix + "data: " + rawPrivacyResponse + "\n\n"))
		require.NoError(t, err)
		require.NotContains(t, string(out), "secret-task")
	}
}

func TestRawRelayPrivacyLimitsAndHeadersOnErrors(t *testing.T) {
	for _, stream := range []bool{false, true} {
		body := strings.Repeat("a", rawRelayPrivacyLimit+1)
		resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
		if stream {
			resp.Header.Set("Content-Type", "text/event-stream")
		}
		require.NoError(t, prepareRawRelayResponsePrivacy(resp))
		out, err := io.ReadAll(resp.Body)
		require.Error(t, err)
		require.Empty(t, out)
		require.NoError(t, resp.Body.Close())
	}
	for _, status := range []int{400, 401, 403, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			const rawError = " {\"error\":{\"code\":\"vendor\",\"message\":\"raw private information\"}}\n"
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for _, name := range rawPrivacyHeaders {
					w.Header().Set(name, "private-response-header")
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, rawError)
			}))
			defer up.Close()
			h, _, router := newModelQuotaTestHandler(t, 100, up.URL, false)
			addRawRelayTestAccount(h, up.URL)
			w := performModelQuotaRequest(router, "/v1/responses", `{"model":"vendor/custom","input":"hi"}`)
			require.Equal(t, status, w.Code)
			require.Equal(t, rawError, w.Body.String())
			require.Equal(t, "7", w.Header().Get("Retry-After"))
			for _, name := range rawPrivacyHeaders {
				require.Empty(t, w.Header().Get(name), name)
			}
		})
	}
	header := http.Header{"sEt-CoOkIe": {"private"}, "x-openai-account-id": {"private"}, "X-Request-Id": {"keep"}}
	clean := rawRelayResponseHeaders(header)
	require.Len(t, clean, 1)
	require.Equal(t, "keep", clean.Get("X-Request-Id"))
	require.Len(t, header, 3, "the upstream diagnostic headers remain intact")
}

func TestRawRelayPrivacyStreamDeliversSanitizedEventBeforeEOF(t *testing.T) {
	for _, gzipStream := range []bool{false, true} {
		t.Run(fmt.Sprint(gzipStream), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			canceled := make(chan struct{})
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				var writer io.Writer = w
				var zipper *gzip.Writer
				if gzipStream {
					w.Header().Set("Content-Encoding", "gzip")
					zipper = gzip.NewWriter(w)
					writer = zipper
				}
				_, _ = io.WriteString(writer, "data: {\"type\":\"response.created\",\"response\":"+rawPrivacyResponse+"}\n\n")
				if zipper != nil {
					_ = zipper.Flush()
				}
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(canceled)
			}))
			defer up.Close()
			h, _, router := newModelQuotaTestHandler(t, 100, up.URL, false)
			addRawRelayTestAccount(h, up.URL)
			server := httptest.NewServer(router)
			defer server.Close()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"vendor/custom","stream":true}`))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+modelQuotaTestKey)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			first := make([]byte, 4096)
			n, err := resp.Body.Read(first)
			require.NoError(t, err)
			require.Contains(t, string(first[:n]), "resp_keep")
			require.NotContains(t, string(first[:n]), "secret-")
			cancel()
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("upstream cancellation was lost")
			}
		})
	}
}

func TestRawRelayPrivacyFragmentedSSELineEndings(t *testing.T) {
	for _, ending := range []string{"\n", "\r\n", "\r"} {
		t.Run(fmt.Sprintf("%q", ending), func(t *testing.T) {
			reader, writer := io.Pipe()
			defer writer.Close()
			resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: reader}
			require.NoError(t, prepareRawRelayResponsePrivacy(resp))
			defer resp.Body.Close()
			frame := "data: " + rawPrivacyResponse + ending + ending
			go func() {
				for _, b := range []byte(frame) {
					if _, err := writer.Write([]byte{b}); err != nil {
						return
					}
				}
				// Leave the stream open: delivering this event cannot depend on EOF.
			}()
			type result struct {
				body []byte
				err  error
			}
			done := make(chan result, 1)
			go func() {
				buf := make([]byte, 4096)
				n, err := resp.Body.Read(buf)
				done <- result{buf[:n], err}
			}()
			select {
			case first := <-done:
				require.NoError(t, first.err)
				require.NotContains(t, string(first.body), "secret-")
				require.Contains(t, string(first.body), "business-task")
				require.True(t, bytes.HasSuffix(first.body, []byte("\n\n")))
			case <-time.After(3 * time.Second):
				t.Fatal("fragmented SSE event waited for EOF")
			}
		})
	}
}
