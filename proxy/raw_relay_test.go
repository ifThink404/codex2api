package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func addRawRelayTestAccount(h *Handler, endpoint string) *auth.Account {
	a := &auth.Account{DBID: 13, UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL: endpoint, APIKey: "upstream-only-key", PlanType: "api",
		Models: []string{"gpt-6-astra", "vendor/custom"}, OpenAIRawPassthrough: true,
		ModelMapping: `{"gpt-6-astra":"must-not-send"}`, CodexClientMetadataMode: "always"}
	h.store.AddAccount(a)
	return a
}

func TestRawRelayPreservesWireAcrossEndpoints(t *testing.T) {
	const body = " {\n\"model\":\"gpt-6-astra\", \"stream\":true, \"service_tier\":\"priority\", \"instructions\":\"keep\", \"input\":[{\"type\":\"function_call\",\"namespace\":\"collaboration\",\"name\":\"list_agents\",\"arguments\":\"{}\"},{\"type\":\"input_image\",\"image_url\":\"data:image/png;base64,AAAA\"}], \"client_metadata\":{\"turn_id\":\"arbitrary\",\"turn_id\":\"duplicate\"}, \"vendor\":9007199254740993 }\n"
	const response = "event: vendor\r\ndata: {\"id\":\"original\",\"model\":\"original-model\",\"namespace\":\"collaboration\"}\r\n\r\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":17,\"output_tokens\":3}}}\n\ndata: [DONE]\n\n"
	var path string
	var sent atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent.Add(1)
		got, _ := io.ReadAll(r.Body)
		if string(got) != body || r.URL.RequestURI() != path {
			t.Errorf("wire changed: path=%s body=%s", r.URL.RequestURI(), got)
		}
		if r.Header.Get("Authorization") != "Bearer upstream-only-key" {
			t.Error("wrong upstream credential")
		}
		for _, name := range []string{"X-Api-Key", "Cookie", "X-Newapi-Signature", "X-Codex2api-Policy", "X-Forwarded-For", "X-Hop"} {
			if r.Header.Get(name) != "" {
				t.Errorf("forwarded local/hop header %s", name)
			}
		}
		if r.Header.Get("Session-Id") != "original-session" || r.Header.Get("User-Agent") != "client-UA" || r.Header.Get("X-Vendor") != "keep" {
			t.Error("client headers changed")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Upstream", "unchanged")
		w.Header().Set("Trailer", "X-Finish")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, response)
		w.Header().Set("X-Finish", "finished")
	}))
	defer up.Close()
	h, _, router := newModelQuotaTestHandler(t, 100, up.URL, false)
	addRawRelayTestAccount(h, up.URL+"/v1")
	for _, endpoint := range []string{"/v1/responses", "/v1/responses/compact", "/v1/chat/completions", "/v1/messages", "/responses"} {
		path = endpoint + "?beta=true&x=a%2Fb"
		if endpoint == "/responses" {
			path = "/v1" + path
		}
		req := httptest.NewRequest(http.MethodPost, endpoint+"?beta=true&x=a%2Fb", strings.NewReader(body))
		req.Header = http.Header{"Authorization": {"Bearer " + modelQuotaTestKey}, "Content-Type": {"application/json"}, "Session-Id": {"original-session"}, "User-Agent": {"client-UA"}, "X-Vendor": {"keep"}, "X-Api-Key": {"local-secondary-key"}, "Cookie": {"local-session"}, "X-Newapi-Signature": {"local-signature"}, "X-Codex2api-Policy": {"local-policy"}, "X-Forwarded-For": {"127.0.0.9"}, "Connection": {"X-Hop"}, "X-Hop": {"must-remove"}}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusCreated || w.Body.String() != response {
			t.Fatalf("%s status=%d body=%s", endpoint, w.Code, w.Body.String())
		}
		if w.Header().Get("X-Upstream") != "unchanged" || w.Result().Trailer.Get("X-Finish") != "finished" {
			t.Fatalf("response headers/trailer changed: %v / %v", w.Header(), w.Result().Trailer)
		}
	}
	if sent.Load() != 5 {
		t.Fatalf("sent %d requests", sent.Load())
	}
}

func TestRawRelayErrorsAndRedirectsNeverRetryOrRewrite(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusTemporaryRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			const response = " {\"error\":{\"code\":\"vendor-code\",\"namespace\":\"original\",\"message\":\"verbatim\"}}\n"
			var sent atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sent.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Location", "/must-not-follow")
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(status)
				io.WriteString(w, response)
			}))
			defer up.Close()
			h, _, r := newModelQuotaTestHandler(t, 100, up.URL, false)
			addRawRelayTestAccount(h, up.URL)
			w := performModelQuotaRequest(r, "/v1/responses", `{"model":"vendor/custom","input":"hi"}`)
			if w.Code != status || w.Body.String() != response || w.Header().Get("Retry-After") != "7" || sent.Load() != 1 {
				t.Fatalf("status=%d sent=%d body=%s", w.Code, sent.Load(), w.Body.String())
			}
		})
	}
}

func TestRawRelayGzipBytesPreserved(t *testing.T) {
	compress := func(s string) []byte {
		var b bytes.Buffer
		z := gzip.NewWriter(&b)
		z.Write([]byte(s))
		z.Close()
		return b.Bytes()
	}
	body := compress(`{"model":"gpt-6-astra","input":"hello"}`)
	response := compress(`{"usage":{"input_tokens":5,"output_tokens":2},"vendor":"untouched"}`)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		if !bytes.Equal(got, body) || r.Header.Get("Content-Encoding") != "gzip" {
			t.Error("compressed request changed")
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/json")
		w.Write(response)
	}))
	defer up.Close()
	h, _, r := newModelQuotaTestHandler(t, 100, up.URL, false)
	addRawRelayTestAccount(h, up.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+modelQuotaTestKey)
	req.Header.Set("Content-Encoding", "gzip")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), response) || w.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("gzip response changed, status=%d", w.Code)
	}
}

func TestRawRelayScopesAndDefaultOff(t *testing.T) {
	h, _, _ := newModelQuotaTestHandler(t, 100, "https://unused.example", false)
	a := addRawRelayTestAccount(h, "https://unused.example")
	for _, scenario := range []string{"disabled_switch", "different_model", "different_key"} {
		t.Run(scenario, func(t *testing.T) {
			a.OpenAIRawPassthrough = scenario != "disabled_switch"
			a.AllowedAPIKeyIDs = nil
			model := "gpt-6-astra"
			if scenario == "different_model" {
				model = "missing-model"
			}
			if scenario == "different_key" {
				a.AllowedAPIKeyIDs = []int64{999}
			}
			body := `{"model":"` + model + `","input":"keep"}`
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
			if h.tryRawRelay(c) {
				t.Fatal("unmatched request hijacked")
			}
			got, err := readRawRequestBody(c)
			if err != nil || string(got) != body {
				t.Fatal("fallback body consumed")
			}
		})
	}
}

func TestRawRelayKeepsModelQuota(t *testing.T) {
	var sent atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent.Add(1)
		io.WriteString(w, `{"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer up.Close()
	h, _, r := newModelQuotaTestHandler(t, 1, up.URL, false)
	addRawRelayTestAccount(h, up.URL)
	for i := 0; i < 2; i++ {
		w := performModelQuotaRequest(r, "/v1/chat/completions", `{"model":"gpt-6-astra","messages":[]}`)
		if i == 0 && w.Code != 200 || i == 1 && w.Code != 429 {
			t.Fatalf("attempt %d status=%d body=%s", i, w.Code, w.Body.String())
		}
	}
	if sent.Load() != 1 {
		t.Fatal("quota exceeded upstream")
	}
}

func TestRawRelayStreamsBeforeCompletionAndCancelsUpstream(t *testing.T) {
	cancelled := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(cancelled)
	}))
	defer up.Close()
	h, _, r := newModelQuotaTestHandler(t, 100, up.URL, false)
	addRawRelayTestAccount(h, up.URL)
	server := httptest.NewServer(r)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-6-astra","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+modelQuotaTestKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	first := make([]byte, len("data: first\n\n"))
	_, err = io.ReadFull(resp.Body, first)
	if err != nil || string(first) != "data: first\n\n" {
		t.Fatalf("first chunk delayed or rewritten: %s %v", first, err)
	}
	cancel()
	resp.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not cancel")
	}
}

func TestRawRelayUsageObservation(t *testing.T) {
	for _, tc := range []struct {
		name, payload                string
		stream                       bool
		input, output, cached, write int
	}{
		{"responses", `{"response":{"usage":{"input_tokens":10,"output_tokens":2,"input_tokens_details":{"cached_tokens":8}}}}`, false, 10, 2, 8, 0},
		{"chat", "data: {\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":3,\"prompt_tokens_details\":{\"cached_tokens\":4}}}", true, 9, 3, 4, 0},
		{"messages", "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5,\"cache_read_input_tokens\":10,\"cache_creation_input_tokens\":3,\"output_tokens\":1}}}\n\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":7}}\n\n", true, 18, 7, 10, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := &rawRelayUsageObserver{stream: tc.stream}
			for _, b := range []byte(tc.payload) {
				o.Write([]byte{b})
			}
			o.finish()
			if o.usage == nil || o.usage.InputTokens != tc.input || o.usage.OutputTokens != tc.output || o.usage.CachedTokens != tc.cached || o.usage.CacheWriteTokens != tc.write {
				t.Fatalf("usage=%+v", o.usage)
			}
		})
	}
}

func TestRawRelayUnavailableDoesNotFallBack(t *testing.T) {
	var sent atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sent.Add(1); io.WriteString(w, `{}`) }))
	defer up.Close()
	h, _, router := newModelQuotaTestHandler(t, 100, up.URL, false)
	a := addRawRelayTestAccount(h, up.URL)
	atomic.StoreInt32(&a.Disabled, 1)
	w := performModelQuotaRequest(router, "/v1/responses", `{"model":"gpt-6-astra","input":"hi"}`)
	if w.Code != 503 || sent.Load() != 0 {
		t.Fatalf("disabled raw account fell back: status=%d sent=%d", w.Code, sent.Load())
	}
}

func TestRawRelayRejectsAmbiguousRoutingModel(t *testing.T) {
	h, _, r := newModelQuotaTestHandler(t, 100, "https://unused.example", false)
	addRawRelayTestAccount(h, "https://unused.example")
	w := performModelQuotaRequest(r, "/v1/responses", `{"model":"gpt-6-astra","model":"not-allowed","input":"hi"}`)
	if w.Code != 400 {
		t.Fatalf("duplicate model status=%d", w.Code)
	}
}

func TestRawRelayUsageGzipAndLimits(t *testing.T) {
	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	z.Write([]byte(`{"model":"real-model","service_tier":"priority","usage":{"input_tokens":5,"output_tokens":2}}`))
	z.Close()
	o := &rawRelayUsageObserver{encoding: "gzip"}
	o.Write(b.Bytes())
	o.finish()
	if o.disabled || o.usage == nil || o.usage.InputTokens != 5 || o.model != "real-model" || o.tier != "priority" {
		t.Fatalf("gzip observation: %+v", o)
	}
	if tiers := resolveUsageServiceTiers(o.tier, ""); tiers.BillingServiceTier == "priority" {
		t.Fatal("default upstream priority raised billing tier")
	}
	o = &rawRelayUsageObserver{stream: true}
	o.Write([]byte("data: "))
	o.Write(bytes.Repeat([]byte("x"), 4<<20))
	o.Write([]byte("\n\ndata: {\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}\n\n"))
	o.finish()
	if o.usage == nil || o.usage.InputTokens != 2 {
		t.Fatal("oversized event hid later usage")
	}
}

func TestRawRelayInvalidProxyDoesNotGoDirect(t *testing.T) {
	a := &auth.Account{DBID: 13999, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://unused.example", APIKey: "test"}
	if client, err := rawRelayClient(a, "unsupported://127.0.0.1:1"); err == nil || client != nil {
		t.Fatal("invalid proxy accepted")
	}
}

func TestRawRelayRecordsAccountUsageAndActualTierWithoutRaisingBill(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"actual-vendor","service_tier":"priority","usage":{"input_tokens":100,"output_tokens":5,"input_tokens_details":{"cached_tokens":80}}}`)
	}))
	defer up.Close()
	h, _, router := newModelQuotaTestHandler(t, 100, up.URL, false)
	addRawRelayTestAccount(h, up.URL)
	h.db.SetUsageLogConfig(database.UsageLogModeFull, 100, 60)
	w := performModelQuotaRequest(router, "/v1/responses", `{"model":"gpt-6-astra","input":"hi"}`)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	h.db.FlushUsageLogs()
	deadline := time.Now().Add(2 * time.Second)
	for {
		logs, err := h.db.ListUsageLogsByTimeRange(context.Background(), time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if len(logs) > 0 {
			log := logs[0]
			if len(logs) != 1 || log.AccountID != 13 || log.InputTokens != 100 || log.CachedTokens != 80 || log.OutputTokens != 5 || log.BillingServiceTier == "priority" || log.RequestedServiceTier != "" || log.ActualServiceTier != "priority" {
				t.Fatalf("wrong accounting: %+v", log)
			}
			detail, err := h.db.GetUsageRequestDiagnostics(context.Background(), log.ID)
			if err != nil {
				t.Fatal(err)
			}
			diagnostic := string(detail.Diagnostics)
			if !gjson.Get(diagnostic, "raw_passthrough.enabled").Bool() || gjson.Get(diagnostic, "raw_passthrough.usage_source").String() != "upstream" || gjson.Get(diagnostic, "selected_account_id").Int() != 13 {
				t.Fatalf("missing raw route diagnostic: %s", diagnostic)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("usage log was not recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRawRelayAliasCannotReenterConversion(t *testing.T) {
	for _, endpoint := range []string{"/v1/responses", "/v1/chat/completions", "/v1/responses/compact", "/v1/messages"} {
		t.Run(endpoint, func(t *testing.T) {
			var sent atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sent.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, modelQuotaSSE)
			}))
			defer up.Close()
			h, _, router := newModelQuotaTestHandler(t, 100, up.URL, false)
			atomic.StoreInt32(&h.store.FindByID(1).Disabled, 1)
			a := addRawRelayTestAccount(h, up.URL)
			a.ModelMapping = `{"team-model":"gpt-6-astra"}`
			body := `{"model":"team-model","input":"hello","messages":[{"role":"user","content":"hello"}],"max_tokens":32}`
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			req := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(body)).WithContext(ctx)
			req.Header.Set("Authorization", "Bearer "+modelQuotaTestKey)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			if sent.Load() != 0 {
				t.Fatalf("raw account received transformed %s request: %s", endpoint, w.Body.String())
			}
			if w.Code < 400 {
				t.Fatalf("unmatched raw-only model must fail: status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestRawRelayAliasKeepsOrdinaryRelayAndExplicitAliasWorking(t *testing.T) {
	for _, declared := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary_relay", true: "explicit_raw_alias"}[declared], func(t *testing.T) {
			var wirePath, wireModel, wireAuthorization string
			var sent atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				wirePath = r.URL.Path
				wireModel = gjson.GetBytes(b, "model").String()
				wireAuthorization = r.Header.Get("Authorization")
				sent.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, modelQuotaSSE)
			}))
			defer up.Close()
			h, _, router := newModelQuotaTestHandler(t, 100, up.URL, false)
			a := addRawRelayTestAccount(h, up.URL)
			a.ModelMapping = `{"team-model":"gpt-6-astra"}`
			if declared {
				a.Models = append(a.Models, "team-model")
			}
			w := performModelQuotaRequest(router, "/v1/chat/completions", `{"model":"team-model","messages":[{"role":"user","content":"hello"}],"stream":true}`)
			if w.Code != 200 || sent.Load() != 1 {
				t.Fatalf("status=%d sent=%d body=%s", w.Code, sent.Load(), w.Body.String())
			}
			if declared {
				if wirePath != "/v1/chat/completions" || wireModel != "team-model" || wireAuthorization != "Bearer upstream-only-key" {
					t.Fatalf("explicit raw alias changed: %s %s %s", wirePath, wireModel, wireAuthorization)
				}
			} else if wirePath != "/v1/responses" || wireModel != "gpt-6-astra" || wireAuthorization != "Bearer relay-test" {
				t.Fatalf("ordinary alias routing changed: %s %s %s", wirePath, wireModel, wireAuthorization)
			}
		})
	}
}
