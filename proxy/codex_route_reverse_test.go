package proxy

import (
	"bytes"
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
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestBPSNativeMigrationRestartsIdentity(t *testing.T) {
	for _, scenario := range []string{"same_account", "other_account", "model_change", "preserve_input", "relaxed_only", "quota_preserve", "migrated_bps"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
			t.Setenv("CODEX_TRANSPORT_MODE", "standard")
			t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
			h, owner, target, key := failoverTestSetup(t, true)
			on := true
			owner.CodexNative, owner.CodexBPS = &on, false
			target.CodexNative, target.CodexBPS = &on, false
			want := owner
			if scenario == "other_account" || scenario == "quota_preserve" {
				atomic.StoreInt32(&owner.Disabled, 1)
				want = target
			}
			if scenario == "quota_preserve" {
				atomic.StoreInt32(&owner.Disabled, 0)
				owner.CodexBPS = true
				owner.UsagePercent7d, owner.UsagePercent7dValid, owner.PlanType, owner.Reset7dAt = 100, true, "free", time.Now().Add(time.Hour)
			}
			if scenario == "model_change" {
				owner.CodexBPS, owner.CodexBPSModels = true, []string{"gpt-6-*"}
			}
			UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings {
				s.CodexSessionFailoverPreserveInput = scenario == "preserve_input" || scenario == "quota_preserve"
				if scenario == "relaxed_only" {
					s.CodexSessionFailoverEnabled, s.CodexForkAccountFallbackEnabled = false, true
				}
				return s
			})
			key += "-reverse"
			h.store.BindSessionAffinity(key, owner, "")
			record := database.SessionContinuityRecord{AccountID: owner.ID(), UpstreamMode: "bps", ThreadID: continuityTestThread, NumberKnown: true, Number: 47, LastSeen: time.Now()}
			if scenario == "migrated_bps" {
				record.FailoverCount, record.OutboundWindowReset, record.LossyContextRestart = 2, true, true
			}
			_, err := h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(key), record)
			require.NoError(t, err)
			c, body := continuityTestRequest(47, "turn")
			c.Request.Header.Set("Authorization", "Bearer test-user-key")
			c.Request.Header.Set("X-Codex-Turn-State", "old-bps-state")
			h.bindCodexIdentityClaims(c)
			body, err = sjson.SetRawBytes(body, "input", []byte(`[{"type":"compaction","encrypted_content":"old-bps-compaction"},{"type":"reasoning","encrypted_content":"old-bps-reasoning"},{"type":"item_reference","id":"old-bps-item"},{"role":"user","content":[{"type":"input_text","text":"current task"},{"type":"input_file","file_id":"old-bps-file"}]}]`))
			require.NoError(t, err)
			body = addSessionTools(t, body)
			body, err = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.turn_id", "01a0939f-d89c-77f1-94fa-080df9ebda48")
			require.NoError(t, err)
			c.Request.Header.Set(codexTurnMetadataHeader, gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").Raw)
			c.Set(ingressRequestBodyContextKey, body)
			// Seed the old account's mapping to prove same-account migration does
			// not reuse its UUIDs or cache partition.
			oldContext := context.WithValue(c.Request.Context(), sessionOutboundEpochContextKey{}, &sessionOutboundEpoch{handler: h, key: hashRiskIdentity(key), record: record, owner: responseCacheOwnerForRequest(c, requestAPIKeyID(c)), upstreamAccount: owner.EffectiveAccountID()})
			if scenario == "migrated_bps" {
				recordSessionContextTokens(oldContext, owner, func(recordToken func(string, string)) {
					recordToken("encrypted_content", "old-bps-compaction")
					recordToken("encrypted_content", "old-bps-reasoning")
					recordToken("file_id", "old-bps-file")
					recordToken("item_reference", "old-bps-item")
				})
				known, cancel := outboundEpochFromContext(oldContext).restartContextVerifier(oldContext)
				require.True(t, known("encrypted_content", "old-bps-compaction"))
				cancel()
			}
			oldFingerprint := NewCodexTransportFingerprint(owner, c.Request.Header, body, "cache", oldContext)
			require.NoError(t, oldFingerprint.ClaimSessionIdentity(oldContext, owner, "test-user-key"))
			require.NotNil(t, oldFingerprint.accountIdentity)
			oldHeaders := c.Request.Header.Clone()
			oldHeaders = oldFingerprint.accountIdentity.rewriteHeaders(oldHeaders)
			oldSession := gjson.Get(oldHeaders.Get(codexTurnMetadataHeader), "session_id").String()
			require.NotEmpty(t, oldSession)
			oldCache := oldFingerprint.ScopeCacheKey(oldContext, "cache")
			require.Nil(t, h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
			oldEpoch := c.Request.Context()
			// Old BPS provenance must not reimpose the old route after a clean
			// migration, but ordinary requests still obey their committed route.
			c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), codexRouteFloorKey{}, "bps"))
			routing, _ := sessionRestartRoutingContext(c, body)
			require.NotContains(t, string(routing), "old-bps-")
			selected, _, handled := h.takeSessionAccountFailover(c.Request.Context(), key, 0, nil, codexRouteAccountFilter(c, nil), auth.DispatchPolicyStandard)
			require.True(t, handled)
			require.Same(t, want, selected)
			h.store.Release(selected)
			committed, found, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, "native", committed.UpstreamMode)
			require.Equal(t, record.FailoverCount+1, committed.FailoverCount)
			require.True(t, committed.OutboundWindowReset)
			require.True(t, committed.LossyContextRestart)
			require.False(t, committed.PreserveRestartInput)
			require.False(t, PreserveSessionInput(c.Request.Context()))
			require.Error(t, validateSessionOutboundEpoch(oldEpoch, owner))
			require.NoError(t, ValidateCodexNativeRoute(c.Request.Context(), selected, body))
			var firstSession, firstCache, firstTurn string
			calls := 0
			installClaudeBoundaryTransport(t, selected, func(r *http.Request) (*http.Response, error) {
				calls++
				require.Equal(t, "https://chatgpt.com/backend-api/codex/responses", r.URL.String())
				out, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				require.NotContains(t, string(out), "old-bps-")
				require.Contains(t, string(out), "current task")
				assertSessionTools(t, out)
				require.Empty(t, r.Header.Get("X-Codex-Turn-State"))
				require.Empty(t, r.Header.Get("X-Bps-Task-Id"))
				require.False(t, gjson.GetBytes(out, "task_id").Exists())
				turn := gjson.Get(r.Header.Get(codexTurnMetadataHeader), "turn_id").String()
				require.NotEmpty(t, turn)
				require.NotEqual(t, gjson.Get(oldHeaders.Get(codexTurnMetadataHeader), "turn_id").String(), turn)
				session := r.Header.Get("Session-Id")
				require.NotEmpty(t, session)
				require.NotEqual(t, oldSession, session)
				require.Equal(t, session, r.Header.Get("Thread-Id"))
				if calls < 3 {
					require.Equal(t, session+":0", r.Header.Get("X-Codex-Window-Id"))
				} else {
					require.Equal(t, session+":1", r.Header.Get("X-Codex-Window-Id"))
				}
				require.NotEmpty(t, gjson.GetBytes(out, "prompt_cache_key").String())
				require.NotEqual(t, oldCache, gjson.GetBytes(out, "prompt_cache_key").String())
				if calls == 1 {
					firstSession, firstCache = session, gjson.GetBytes(out, "prompt_cache_key").String()
					firstTurn = turn
				} else {
					require.Equal(t, firstTurn, turn)
					require.Equal(t, firstSession, session)
					require.Equal(t, firstCache, gjson.GetBytes(out, "prompt_cache_key").String())
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"resp_native","output":[]}`)), Request: r}, nil
			})
			for range 2 {
				resp, err := ExecuteRequest(c.Request.Context(), selected, body, "cache", "", "test-user-key", nil, c.Request.Header, false)
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
			}
			require.Equal(t, 2, calls)
			// Loading the committed root must keep the new segment stable.
			h.continuityRecords = nil
			resumed, _ := continuityTestRequest(48, "turn")
			resumed.Request.Header.Set("Authorization", "Bearer test-user-key")
			h.bindCodexIdentityClaims(resumed)
			raw, err := sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.window_id", continuityTestThread+":48")
			require.NoError(t, err)
			resumed.Request.Header.Set(codexTurnMetadataHeader, gjson.GetBytes(raw, "client_metadata.x-codex-turn-metadata").Raw)
			require.Nil(t, h.configureSessionModelAffinity(resumed, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, raw))
			require.Equal(t, "native", outboundEpochFromContext(resumed.Request.Context()).record.UpstreamMode)
			require.Equal(t, outboundEpochFromContext(c.Request.Context()).identityKey(), outboundEpochFromContext(resumed.Request.Context()).identityKey())
			require.Nil(t, h.commitSessionContinuity(resumed, selected))
			resp, err := ExecuteRequest(resumed.Request.Context(), selected, raw, "cache", "", "test-user-key", nil, resumed.Request.Header, false)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, 3, calls)
		})
	}
}

func TestBPSNativeMigrationIngress(t *testing.T) {
	for _, compact := range []bool{false, true} {
		t.Run(map[bool]string{false: "responses", true: "compact"}[compact], func(t *testing.T) {
			t.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
			t.Setenv("CODEX_TRANSPORT_MODE", "standard")
			t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
			h, owner, _, _ := failoverTestSetup(t, true)
			on := true
			owner.CodexNative, owner.CodexBPS = &on, false
			c, body := failoverTestRequest(t, h)
			body, _ = sjson.SetRawBytes(body, "input", []byte(`[{"type":"compaction","encrypted_content":"old-bps-compaction"},{"role":"user","content":"current task"}]`))
			body, _ = sjson.SetBytes(body, "stream", !compact)
			path := "/v1/responses"
			if compact {
				path += "/compact"
				body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.request_kind", "compaction")
			}
			c.Set(contextAPIKeyID, int64(101))
			c.Set(ingressRequestBodyContextKey, body)
			identity := h.resolveRequestSessionIdentityForContext(c, body)
			key := capacityAwareSessionAffinityKey(identity, 101)
			_, err := h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(key), database.SessionContinuityRecord{AccountID: owner.ID(), ThreadID: continuityTestThread, NumberKnown: true, LastSeen: time.Now(), UpstreamMode: "bps"})
			require.NoError(t, err)
			h.store.BindSessionAffinity(key, owner, "")
			// Known BPS compaction must be removed before provenance filtering.
			bpsContext := context.WithValue(t.Context(), sessionOutboundEpochContextKey{}, &sessionOutboundEpoch{record: database.SessionContinuityRecord{AccountID: owner.ID(), UpstreamMode: "bps"}})
			require.NoError(t, h.recordCompactionProvenance(bpsContext, owner, "old-bps-compaction"))
			calls := 0
			installClaudeBoundaryTransport(t, owner, func(r *http.Request) (*http.Response, error) {
				calls++
				require.Equal(t, "chatgpt.com", r.URL.Host)
				require.Equal(t, "/backend-api/codex"+strings.TrimPrefix(path, "/v1"), r.URL.Path)
				out, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				require.NotContains(t, string(out), "old-bps-")
				require.Contains(t, string(out), "current task")
				w := httptest.NewRecorder()
				if compact {
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{"id":"native-compact","object":"response.compaction","output":[{"type":"compaction","id":"native-item","encrypted_content":"native-encrypted"}]}`)
				} else {
					stickyFailureSuccess(w)
				}
				return w.Result(), nil
			})
			c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
			c.Request.Header.Set("Authorization", "Bearer test-user-key")
			ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
			defer cancel()
			c.Request = c.Request.WithContext(ctx)
			if compact {
				h.ResponsesCompact(c)
			} else {
				h.Responses(c)
			}
			require.Equal(t, 200, c.Writer.Status())
			require.Equal(t, 1, calls)
			diagnostic := usageRequestDiagnosticState(c).AccountFailover
			require.NotNil(t, diagnostic)
			require.Equal(t, "switched", diagnostic.Result)
			require.Equal(t, "native", diagnostic.UpstreamMode)
			require.True(t, diagnostic.OriginalAccountNativeAttempted)
		})
	}
}

func TestBPSNativeMissingOwnerMigrates(t *testing.T) {
	h, ownerID, target, key, c, body := missingOwnerSetup(t, "deleted")
	on := true
	target.CodexBPS, target.CodexNative = false, &on
	require.Nil(t, h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
	selected, _, handled := h.takeSessionAccountFailover(c.Request.Context(), key, 101, nil, codexRouteAccountFilter(c, sessionModelSupportFilter("gpt-5.6-sol", "gpt-5.6-sol", false)), auth.DispatchPolicyStandard)
	require.True(t, handled)
	require.Same(t, target, selected)
	h.store.Release(selected)
	record, _, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
	require.NoError(t, err)
	require.Equal(t, ownerID, record.PreviousAccountID)
	require.Equal(t, target.ID(), record.AccountID)
	require.Equal(t, "native", record.UpstreamMode)
	require.Equal(t, uint64(1), record.FailoverCount)
	require.True(t, record.OutboundWindowReset)
	require.False(t, record.PreserveRestartInput)
}

func TestBPSNativeMigrationDoesNotBypassFilters(t *testing.T) {
	for _, scenario := range []string{"failover_off", "native_model_mismatch", "request_filter", "key_scope", "opaque_only", "healthy_bps"} {
		t.Run(scenario, func(t *testing.T) {
			h, owner, target, key := failoverTestSetup(t, scenario != "failover_off")
			on := true
			owner.CodexNative, target.CodexNative = &on, &on
			if scenario == "healthy_bps" {
				owner.CodexBPS = true
			}
			if scenario == "native_model_mismatch" {
				owner.CodexNativeModels, target.CodexNativeModels = []string{"gpt-6-*"}, []string{"gpt-6-*"}
			}
			key += "-reverse"
			h.store.BindSessionAffinity(key, owner, "")
			_, err := h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(key), database.SessionContinuityRecord{AccountID: owner.ID(), UpstreamMode: "bps", ThreadID: continuityTestThread, NumberKnown: true, LastSeen: time.Now()})
			require.NoError(t, err)
			c, body := failoverTestRequest(t, h)
			if scenario == "key_scope" {
				c.Set(contextAPIKeyID, int64(101))
				h.store.SetAPIKeyAllowedGroups(101, []int64{99})
			}
			if scenario == "opaque_only" {
				body, _ = sjson.SetRawBytes(body, "input", []byte(`[{"type":"compaction","encrypted_content":"old-bps-only"}]`))
			}
			failure := h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body)
			if scenario == "opaque_only" {
				require.NotNil(t, failure)
			} else {
				require.Nil(t, failure)
				filter := codexRouteAccountFilter(c, func(a *auth.Account) bool { return scenario != "request_filter" })
				selected, _, _ := h.takeSessionAccountFailover(c.Request.Context(), key, requestAPIKeyID(c), nil, filter, auth.DispatchPolicyStandard)
				require.Nil(t, selected)
			}
			record, _, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
			require.NoError(t, err)
			require.Equal(t, "bps", record.UpstreamMode)
			require.Zero(t, record.FailoverCount)
		})
	}
}
