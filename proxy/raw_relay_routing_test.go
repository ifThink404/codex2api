package proxy

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func rawRoutingTestContext(row *database.APIKeyRow, path string, body []byte, headers http.Header) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	c.Request.Header = headers.Clone()
	if c.Request.Header == nil {
		c.Request.Header = http.Header{}
	}
	c.Request.Header.Set("Authorization", "Bearer "+modelQuotaTestKey)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(contextAPIKeyID, row.ID)
	c.Set(contextAPIKeyRow, row)
	return c, w
}

func configureRawRoutingTestGroups(h *Handler, row *database.APIKeyRow, unrestricted bool) {
	row.AllowedGroupIDs = []int64{10}
	if unrestricted {
		row.AllowedGroupIDs = nil
	}
	row.Limits.NoAffinityGroupIDs = []int64{20}
	h.store.SetAPIKeyAllowedGroups(row.ID, row.AllowedGroupIDs)
	h.store.SetAPIKeyNoAffinityGroups(row.ID, row.Limits.NoAffinityGroupIDs)
	h.store.FindByID(1).GroupIDs = []int64{10}
}

func TestRawRelayGroupRoutingBeforeTransport(t *testing.T) {
	for _, tc := range []struct {
		name, path                  string
		fingerprint, rawPrimary     bool
		bound, missingOwner, paused bool
		unrestricted                bool
		want                        string
		status                      int
	}{
		{name: "codex_returns_to_ordinary_pool", path: "/v1/responses", fingerprint: true, want: "ordinary", status: 200},
		{name: "unrestricted_excludes_split", path: "/v1/responses", fingerprint: true, unrestricted: true, want: "ordinary", status: 200},
		{name: "codex_raw_in_primary", path: "/v1/responses", fingerprint: true, rawPrimary: true, want: "primary", status: 200},
		{name: "no_identity_uses_split", path: "/v1/responses", rawPrimary: true, want: "split", status: 200},
		{name: "chat_path_precedes_fingerprint", path: "/v1/chat/completions", fingerprint: true, rawPrimary: true, want: "split", status: 200},
		{name: "bound_chat_stays_primary", path: "/v1/chat/completions", fingerprint: true, rawPrimary: true, bound: true, want: "primary", status: 200},
		{name: "bound_chat_ordinary_not_preempted", path: "/v1/chat/completions", fingerprint: true, bound: true, want: "ordinary", status: 200},
		{name: "missing_chat_owner_does_not_escape", path: "/v1/chat/completions", fingerprint: true, rawPrimary: true, bound: true, missingOwner: true, status: 503},
		{name: "unavailable_split_does_not_fall_back", path: "/v1/responses", rawPrimary: true, paused: true, status: 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(" {\n\"model\":\"gpt-6-astra\",\"stream\":true,\"input\":\"hello\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}],\"vendor\":9007199254740993}\n")
			var seen []string
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				kind := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				if kind == "relay-test" {
					kind = "ordinary"
				} else {
					got, _ := io.ReadAll(r.Body)
					if !bytes.Equal(got, body) {
						t.Error("raw request bytes changed during routing")
					}
					if tc.fingerprint && r.Header.Get("Session-Id") != testRootSessionA {
						t.Error("raw session header changed")
					}
				}
				seen = append(seen, kind)
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, modelQuotaSSE)
			}))
			defer up.Close()
			h, row, _ := newModelQuotaTestHandler(t, 100, up.URL, false)
			configureRawRoutingTestGroups(h, row, tc.unrestricted)
			split := addRawRelayTestAccount(h, up.URL)
			split.GroupIDs, split.APIKey = []int64{20}, "split"
			if tc.paused {
				split.Disabled = 1
			}
			owner := int64(1)
			if tc.rawPrimary {
				h.store.AddAccount(&auth.Account{DBID: 14, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: up.URL, APIKey: "primary", PlanType: "api", Models: []string{"gpt-6-astra"}, GroupIDs: []int64{10}, OpenAIRawPassthrough: true})
				owner = 14
			}
			if tc.bound {
				if tc.missingOwner {
					owner = 999
				}
				_, err := h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(sessionAffinityKey(testRootSessionA, row.ID)), database.SessionContinuityRecord{AccountID: owner, ThreadID: testRootSessionA, LastSeen: time.Now()})
				require.NoError(t, err)
			}
			headers := http.Header{}
			if tc.fingerprint {
				headers = nativeSessionHeaders(testRootSessionA, testRootSessionA, 0)
			}
			c, w := rawRoutingTestContext(row, tc.path, body, headers)
			if tc.path == "/v1/chat/completions" {
				h.ChatCompletions(c)
			} else {
				h.Responses(c)
			}
			require.Equal(t, tc.status, w.Code, w.Body.String())
			if tc.want == "" {
				require.Empty(t, seen)
			} else {
				require.Equal(t, []string{tc.want}, seen)
			}
			state := usageRequestDiagnosticState(c)
			require.NotNil(t, state.GroupRouting)
			if tc.want == "ordinary" {
				require.Nil(t, state.RawPassthrough, "declined raw route must not mark ordinary requests as passthrough")
			}
		})
	}
}

func TestRawRelayCapturesNativeClassificationWithoutChangingWire(t *testing.T) {
	for _, tc := range []struct {
		name, source, want  string
		child, gzip, uaOnly bool
	}{
		{name: "user", source: "user", want: "user"},
		{name: "gzip_user", source: "user", want: "user", gzip: true},
		{name: "guardian", source: "guardian_review", want: "related_internal", child: true},
		{name: "ua_alone_is_not_a_user_turn", want: "unknown", uaOnly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := nativeSessionHeaders(testRootSessionA, testRootSessionA, 0)
			leaf := testRootSessionA
			if tc.child {
				leaf = testLeafSessionA
				headers = nativeSessionHeaders(testRootSessionA, leaf, 0)
			}
			body := []byte(`{"model":"gpt-6-astra","input":"hello","client_metadata":{"x-codex-turn-metadata":{"session_id":"` + testRootSessionA + `","thread_id":"` + leaf + `","thread_source":"` + tc.source + `","request_kind":"turn","turn_id":"` + testLeafSessionB + `"}}}`)
			if tc.uaOnly {
				headers, body = http.Header{}, []byte(`{"model":"gpt-6-astra","input":"hello"}`)
			}
			headers.Set("User-Agent", "Codex Desktop/0.155.0-alpha.16.4")
			if tc.gzip {
				var compressed bytes.Buffer
				z := gzip.NewWriter(&compressed)
				_, err := z.Write(body)
				require.NoError(t, err)
				require.NoError(t, z.Close())
				body = compressed.Bytes()
				headers.Set("Content-Encoding", "gzip")
			}
			const response = " {\"usage\":{\"input_tokens\":3,\"output_tokens\":1},\"vendor\":\"unchanged\"}\n"
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, _ := io.ReadAll(r.Body)
				if !bytes.Equal(got, body) || r.Header.Get("Session-Id") != headers.Get("Session-Id") || r.Header.Get("Content-Encoding") != headers.Get("Content-Encoding") {
					t.Error("classification changed the wire request")
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, response)
			}))
			defer up.Close()
			h, row, _ := newModelQuotaTestHandler(t, 100, up.URL, false)
			addRawRelayTestAccount(h, up.URL)
			h.db.SetUsageLogConfig(database.UsageLogModeFull, 100, 60)
			c, w := rawRoutingTestContext(row, "/v1/responses", body, headers)
			require.True(t, h.tryRawRelay(c))
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.Equal(t, response, w.Body.String())
			h.db.FlushUsageLogs()
			require.Eventually(t, func() bool {
				logs, err := h.db.ListUsageLogsByTimeRange(t.Context(), time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
				if err != nil || len(logs) != 1 {
					return false
				}
				require.Equal(t, tc.want, logs[0].RequestType, "persisted usage must contain the observed classification")
				return true
			}, 2*time.Second, 10*time.Millisecond)
			input := &database.UsageLogInput{AccountID: 13}
			populateUsageRequestDiagnostics(c, input)
			require.Equal(t, tc.want, input.RequestType)
			snapshot := readUsageDiagnosticSnapshot(t, input)
			require.Equal(t, "captured", snapshot.CaptureStatus)
			require.Equal(t, headers.Get("User-Agent"), snapshot.Incoming["headers"]["User-Agent"])
			if !tc.uaOnly {
				require.Equal(t, leaf, snapshot.Incoming["client_metadata.x-codex-turn-metadata"]["thread_id"])
				require.Equal(t, tc.source, snapshot.Resolved.ThreadSource)
			}
			require.Nil(t, relaxedAccountFallbackFromContext(c.Request.Context()))
		})
	}
}

func TestRawRelaySignedNewAPIRoutingAndClassification(t *testing.T) {
	for _, tc := range []struct {
		name, rootState, source, relation, wantType, wantAccount string
		invalid, native                                          bool
	}{
		{name: "signed_user_without_native_headers", rootState: "resolved", source: "user", relation: "root", wantType: "user", wantAccount: "primary"},
		{name: "signed_background", rootState: "resolved", source: "subagent", relation: "related", wantType: "related_internal", wantAccount: "primary"},
		{name: "signed_unavailable_remains_authoritative", rootState: "unavailable", source: "user", native: true, wantType: "unknown", wantAccount: "split"},
		{name: "invalid_signature_cannot_select_primary", rootState: "resolved", source: "user", relation: "root", invalid: true, wantType: "unknown", wantAccount: "split"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"gpt-6-astra","input":"hello"}`)
			var seen string
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				got, _ := io.ReadAll(r.Body)
				if !bytes.Equal(body, got) || r.Header.Get("X-NewAPI-Signature") != "" {
					t.Error("signed routing altered payload or forwarded local signature")
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			defer up.Close()
			h, row, _ := newModelQuotaTestHandler(t, 100, up.URL, false)
			configureRawRoutingTestGroups(h, row, false)
			cfg := promptGuardTestConfig()
			cfg.Advanced.NewAPI.Enabled = true
			h.store.SetPromptFilterConfig(cfg)
			h.SetRuntimeCache(cache.NewMemory(1))
			h.store.ReplacePromptFilterNewAPIBindings([]*database.PromptFilterNewAPIBinding{{APIKeyID: row.ID, PlatformCode: "test-platform", Secret: "integration-secret", Enabled: true, PolicyMode: database.PromptFilterPolicyModeInherit, PolicyProfile: database.PromptFilterPolicyProfileInherit}})
			split := addRawRelayTestAccount(h, up.URL)
			split.GroupIDs, split.APIKey = []int64{20}, "split"
			h.store.AddAccount(&auth.Account{DBID: 14, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: up.URL, APIKey: "primary", PlanType: "api", Models: []string{"gpt-6-astra"}, GroupIDs: []int64{10}, OpenAIRawPassthrough: true})
			c, w := signedNewAPIPolicyContext(t, "202609260100000000000000rawRouting", newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, "/v1/responses", body)
			c.Set(contextAPIKeyID, row.ID)
			c.Set(contextAPIKeyRow, row)
			meta := newAPIPolicyMeta{Profile: "balanced", Mode: "enforce", Provider: "openai", Protocol: "responses", RootSessionVersion: 1, RootSessionState: tc.rootState, RootSessionRelation: tc.relation, ThreadSource: tc.source, RequestKind: "turn"}
			if tc.rootState == "resolved" {
				meta.RootSessionFingerprint = newAPIRootSessionFingerprint("test-platform", "42", testRootSessionA)
			}
			addSignedNewAPIPolicyMeta(t, c, meta, !tc.invalid)
			if tc.native {
				c.Request.Header = cloneHeaderWithNewAPIIdentity(c.Request.Header, nativeSessionHeaders(testRootSessionA, testRootSessionA, 0))
			}
			require.True(t, h.tryRawRelay(c))
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.Equal(t, tc.wantAccount, seen)
			input := &database.UsageLogInput{}
			populateUsageRequestDiagnostics(c, input)
			require.Equal(t, tc.wantType, input.RequestType)
			snapshot := readUsageDiagnosticSnapshot(t, input)
			if !tc.invalid {
				require.Equal(t, "signed_newapi", snapshot.Resolved.IdentitySource)
				require.Equal(t, c.GetHeader("X-NewAPI-Request-ID"), snapshot.NewAPIRequestID)
				require.Equal(t, tc.source, snapshot.Incoming["signed_newapi"]["thread_source"])
			} else {
				require.Empty(t, snapshot.Incoming["signed_newapi"])
			}
		})
	}
}
