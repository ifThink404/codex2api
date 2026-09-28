package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func stickyFailureSuccess(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_sticky\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
}

func newStickyFailureHarness(t *testing.T, maxRetries, max429Retries int, upstreamA, upstreamB http.Handler) (*Handler, *auth.Store, *auth.Account, *auth.Account, string, func()) {
	t.Helper()
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.RateLimitRetryPolicy = "sticky"; return s })
	serverA := httptest.NewServer(upstreamA)
	serverB := httptest.NewServer(upstreamB)
	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      2,
		MaxRetries:          maxRetries,
		MaxRateLimitRetries: max429Retries,
		TestConcurrency:     1,
		TestModel:           "gpt-5.6-sol",
	})
	store.SetMaxRetries(maxRetries)
	store.SetMaxRateLimitRetries(max429Retries)
	store.SetTransportRetryPolicy("sticky")
	accountA := &auth.Account{DBID: 99101, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: serverA.URL, APIKey: "a", Models: []string{"gpt-5.6-sol"}}
	accountB := &auth.Account{DBID: 99102, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: serverB.URL, APIKey: "b", Models: []string{"gpt-5.6-sol"}}
	useCodexHTTPTestAccounts(t, accountA, accountB)
	store.AddAccount(accountA)
	store.AddAccount(accountB)
	body := []byte(`{"model":"gpt-5.6-sol","input":"sticky probe","stream":true}`)
	headers := http.Header{"Content-Type": []string{"application/json"}, "Session-Id": []string{"sticky-failure-session"}}
	key := capacityAwareSessionAffinityKey(resolveRequestSessionIdentity(headers, body), 0)
	store.BindSessionAffinity(key, accountA, "")
	cleanup := func() {
		serverA.Close()
		serverB.Close()
	}
	return NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil), store, accountA, accountB, key, cleanup
}

func runStickyFailureRequest(t *testing.T, handler *Handler) *httptest.ResponseRecorder {
	t.Helper()
	body := []byte(`{"model":"gpt-5.6-sol","input":"sticky probe","stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Session-Id", "sticky-failure-session")
	req.Header.Set("Authorization", "Bearer sticky-test-client")
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = req
	handler.Responses(c)
	return recorder
}

func TestStickyFailureHTTPStatusesRetrySameAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var callsA, callsB atomic.Int32
			handler, store, accountA, _, key, cleanup := newStickyFailureHarness(t, 3, 3,
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					n := callsA.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					if n <= 3 {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(status)
						_, _ = io.WriteString(w, `{"error":{"message":"temporary probe"}}`)
						return
					}
					stickyFailureSuccess(w)
				}),
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					callsB.Add(1)
					stickyFailureSuccess(w)
				}),
			)
			defer cleanup()

			recorder := runStickyFailureRequest(t, handler)
			if recorder.Code != http.StatusOK || callsA.Load() != 4 || callsB.Load() != 0 {
				t.Fatalf("status %d: downstream=%d A=%d B=%d body=%s", status, recorder.Code, callsA.Load(), callsB.Load(), recorder.Body.String())
			}
			if boundID, ok := store.SessionAffinityAccountID(key); !ok || boundID != accountA.ID() {
				t.Fatalf("status %d: final affinity=%v/%d, want A=%d", status, ok, boundID, accountA.ID())
			}
		})
	}
}

func TestStickyFailureZeroRetriesRetainsAffinity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var callsA, callsB atomic.Int32
	handler, store, accountA, _, key, cleanup := newStickyFailureHarness(t, 0, 0,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if callsA.Add(1) > 1 {
				stickyFailureSuccess(w)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"temporary probe"}}`)
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			callsB.Add(1)
			stickyFailureSuccess(w)
		}),
	)
	defer cleanup()
	recorder := runStickyFailureRequest(t, handler)
	if recorder.Code != http.StatusInternalServerError || callsA.Load() != 1 || callsB.Load() != 0 {
		t.Fatalf("downstream=%d A=%d B=%d body=%s", recorder.Code, callsA.Load(), callsB.Load(), recorder.Body.String())
	}
	if boundID, ok := store.SessionAffinityAccountID(key); !ok || boundID != accountA.ID() {
		t.Fatalf("final affinity=%v/%d, want A=%d", ok, boundID, accountA.ID())
	}
	second := runStickyFailureRequest(t, handler)
	if second.Code != http.StatusOK || callsA.Load() != 2 || callsB.Load() != 0 {
		t.Fatalf("client retry migrated: downstream=%d A=%d B=%d body=%s", second.Code, callsA.Load(), callsB.Load(), second.Body.String())
	}
}

func TestRequestScoped400RetainsAffinityWithoutRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var callsA, callsB atomic.Int32
	handler, store, accountA, _, key, cleanup := newStickyFailureHarness(t, 3, 3,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			callsA.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"bad input"}}`)
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { callsB.Add(1); stickyFailureSuccess(w) }),
	)
	defer cleanup()
	recorder := runStickyFailureRequest(t, handler)
	if recorder.Code != http.StatusBadRequest || callsA.Load() != 1 || callsB.Load() != 0 {
		t.Fatalf("downstream=%d A=%d B=%d body=%s", recorder.Code, callsA.Load(), callsB.Load(), recorder.Body.String())
	}
	if boundID, ok := store.SessionAffinityAccountID(key); !ok || boundID != accountA.ID() {
		t.Fatalf("final affinity=%v/%d, want A=%d", ok, boundID, accountA.ID())
	}
}

func TestStickyFailurePermanentAccountErrorKeepsSessionOwner(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var callsA, callsB atomic.Int32
	handler, _, accountA, _, key, cleanup := newStickyFailureHarness(t, 3, 3,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			callsA.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"invalid token"}}`)
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			callsB.Add(1)
			stickyFailureSuccess(w)
		}),
	)
	defer cleanup()
	recorder := runStickyFailureRequest(t, handler)
	if recorder.Code == http.StatusOK || callsA.Load() != 1 || callsB.Load() != 0 {
		t.Fatalf("downstream=%d A=%d B=%d body=%s", recorder.Code, callsA.Load(), callsB.Load(), recorder.Body.String())
	}
	entry, found, err := handler.readSessionContinuity(context.Background(), hashRiskIdentity(key))
	if err != nil || !found || entry.Record.AccountID != accountA.ID() {
		t.Fatalf("permanent owner=%+v, found=%v, err=%v, want A=%d", entry.Record, found, err, accountA.ID())
	}
}

func TestRotatePolicyCannotSwitchStableSessionOnTemporaryFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var callsA, callsB atomic.Int32
	handler, store, accountA, _, key, cleanup := newStickyFailureHarness(t, 3, 3,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			callsA.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":{"message":"temporary"}}`)
		}),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { callsB.Add(1); stickyFailureSuccess(w) }),
	)
	defer cleanup()
	store.SetTransportRetryPolicy("rotate")
	recorder := runStickyFailureRequest(t, handler)
	if recorder.Code == http.StatusOK || callsA.Load() < 1 || callsB.Load() != 0 {
		t.Fatalf("downstream=%d A=%d B=%d body=%s", recorder.Code, callsA.Load(), callsB.Load(), recorder.Body.String())
	}
	entry, found, err := handler.readSessionContinuity(context.Background(), hashRiskIdentity(key))
	if err != nil || !found || entry.Record.AccountID != accountA.ID() {
		t.Fatalf("permanent owner=%+v, found=%v, err=%v, want A=%d", entry.Record, found, err, accountA.ID())
	}
}

func TestStickyFailureStreamBreakStopsAndExplicitFailureRetriesSameAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := map[string]func(http.ResponseWriter, int32){
		"early EOF": func(w http.ResponseWriter, n int32) {
			w.Header().Set("Content-Type", "text/event-stream")
			if n > 3 {
				stickyFailureSuccess(w)
			}
		},
		"response.failed 500": func(w http.ResponseWriter, n int32) {
			if n > 3 {
				stickyFailureSuccess(w)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"temporary\"}}}\n\n")
		},
	}
	for name, writeA := range cases {
		t.Run(name, func(t *testing.T) {
			var callsA, callsB atomic.Int32
			handler, store, accountA, _, key, cleanup := newStickyFailureHarness(t, 3, 3,
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeA(w, callsA.Add(1)) }),
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { callsB.Add(1); stickyFailureSuccess(w) }),
			)
			defer cleanup()
			recorder := runStickyFailureRequest(t, handler)
			wantStatus, wantCalls := http.StatusOK, int32(4)
			if name == "early EOF" {
				wantStatus, wantCalls = http.StatusBadGateway, 1
			}
			if recorder.Code != wantStatus || callsA.Load() != wantCalls || callsB.Load() != 0 {
				t.Fatalf("downstream=%d A=%d B=%d body=%s", recorder.Code, callsA.Load(), callsB.Load(), recorder.Body.String())
			}
			if boundID, ok := store.SessionAffinityAccountID(key); !ok || boundID != accountA.ID() {
				t.Fatalf("final affinity=%v/%d, want A=%d", ok, boundID, accountA.ID())
			}
		})
	}
}
