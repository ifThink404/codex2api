package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
)

func TestGroupRoutingNativeRetryUsesModeScope(t *testing.T) {
	for _, relaxed := range []bool{false, true} {
		for _, tc := range []struct {
			name                     string
			available, complete, bps bool
		}{
			{"split_exhausted", false, false, false},
			{"split_candidate_available", true, false, false},
			{"resumed_complete_fields_exhausted", false, true, false},
			{"resumed_complete_fields_candidate", true, true, false},
			{"bps_split_exhausted", false, false, true},
			{"bps_split_candidate_available", true, false, true},
			{"bps_resumed_complete_fields_exhausted", false, true, true},
			{"bps_resumed_complete_fields_candidate", true, true, true},
		} {
			t.Run(tc.name+map[bool]string{false: "/strict", true: "/relaxed"}[relaxed], func(t *testing.T) {
				h, owner, outside, key := failoverTestSetup(t, true)
				owner.GroupIDs, outside.GroupIDs = []int64{20}, []int64{10}
				outside.SchedulerPriority = 100
				UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = relaxed; return s })
				c, body := failoverTestRequest(t, h)
				if tc.complete {
					body, _ = sjson.SetBytes(body, "reasoning.effort", "high")
				}
				row := &database.APIKeyRow{ID: 101, AllowedGroupIDs: []int64{10}, Limits: database.APIKeyLimits{NoAffinityGroupIDs: []int64{20}}}
				c.Set(contextAPIKeyID, row.ID)
				c.Set(contextAPIKeyRow, row)
				h.store.SetAPIKeyAllowedGroups(row.ID, row.AllowedGroupIDs)
				h.store.SetAPIKeyNoAffinityGroups(row.ID, row.Limits.NoAffinityGroupIDs)
				inside := &auth.Account{DBID: 1697, AccountID: "861373c1-f1a9-4ca9-8682-a0594b30c36c", AccessToken: "inside-token", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}, GroupIDs: []int64{20}}
				if tc.bps {
					off := false
					for _, account := range []*auth.Account{owner, outside, inside} {
						account.CodexNative, account.CodexBPS = &off, true
					}
				}
				if tc.available {
					h.store.AddAccount(inside)
				}
				captureGroupRoutingIngress(c, body)
				identity := requestSessionIdentity{stableIdentity: true, affinityID: continuityTestThread, hasRequestFingerprint: true}
				require.Nil(t, h.configureSessionModelAffinity(c, identity, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
				filter := applyAffinityGroupRouting(c, identity, nil)
				exclusions := newSessionRetryAccountExclusions(c, key, body)
				exclusions.MarkHTTPFailure(owner.ID(), 429, []byte(`{"error":{"type":"rate_limit_exceeded"}}`), 1, 1)
				selected, _, _ := h.nextRetryAccountForSessionWithDispatchGuard(c.Request.Context(), key, row.ID, exclusions, filter, auth.DispatchPolicyStandard)
				var selectedID int64
				if selected != nil {
					selectedID = selected.ID()
					h.store.Release(selected)
				}
				if relaxed {
					require.Equal(t, outside.ID(), selectedID, "relaxed retry must consider the healthy Key-authorized primary account")
				} else if tc.available && !tc.complete {
					require.Equal(t, inside.ID(), selectedID, "strict retry stays in the original group")
				} else {
					require.Zero(t, selectedID, "exhausted split accounts must not expand to the primary group")
				}
				record, _, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
				require.NoError(t, err)
				if relaxed {
					require.Equal(t, outside.ID(), record.AccountID)
				} else if tc.available && !tc.complete {
					require.Equal(t, inside.ID(), record.AccountID)
				} else {
					require.Equal(t, owner.ID(), record.AccountID)
				}
			})
		}
	}
}

func TestNoSplitGroupStillDispatchesActualRequests(t *testing.T) {
	for _, transport := range []string{"native", "compat", "raw"} {
		for _, relaxed := range []bool{false, true} {
			for _, fields := range []string{"complete", "missing_session", "missing_effort", "both_missing"} {
				name := transport + "/" + map[bool]string{false: "strict", true: "relaxed"}[relaxed] + "/" + fields
				t.Run(name, func(t *testing.T) {
					var calls atomic.Int32
					up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						require.Equal(t, "Bearer relay-test", r.Header.Get("Authorization"))
						w.Header().Set("Content-Type", "text/event-stream")
						io.WriteString(w, modelQuotaSSE)
					}))
					defer up.Close()
					h, row, _ := newModelQuotaTestHandler(t, 100, up.URL, transport == "native")
					row.AllowedGroupIDs = []int64{10}
					h.store.SetAPIKeyAllowedGroups(row.ID, row.AllowedGroupIDs)
					account := h.store.FindByID(1)
					account.GroupIDs = []int64{10}
					account.OpenAIRawPassthrough = transport == "raw"
					if transport == "native" {
						installClaudeBoundaryTransport(t, account, func(r *http.Request) (*http.Response, error) {
							calls.Add(1)
							return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(modelQuotaSSE)), Request: r}, nil
						})
					}
					UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = relaxed; return s })
					body := []byte(`{"model":"gpt-6-astra","input":"hello","stream":true}`)
					if fields == "complete" || fields == "missing_session" {
						body = []byte(`{"model":"gpt-6-astra","input":"hello","stream":true,"reasoning":{"effort":"low"}}`)
					}
					headers := http.Header{}
					if fields == "complete" || fields == "missing_effort" {
						headers = nativeSessionHeaders(testRootSessionA, testRootSessionA, 0)
					}
					for _, afterDisable := range []bool{false, true} {
						if afterDisable {
							// Warm the split configuration, then disable it as a settings update does.
							row.Limits.NoAffinityGroupIDs = []int64{20}
							h.store.SetAPIKeyNoAffinityGroups(row.ID, row.Limits.NoAffinityGroupIDs)
							row.Limits.NoAffinityGroupIDs = nil
							h.store.SetAPIKeyNoAffinityGroups(row.ID, nil)
						}
						c, w := rawRoutingTestContext(row, "/v1/responses", body, headers)
						h.Responses(c)
						require.Equal(t, 200, w.Code, w.Body.String())
						require.Contains(t, w.Body.String(), "response.completed")
					}
					require.EqualValues(t, 2, calls.Load(), "disabled splitting must still dispatch requests in the allowed group")
				})
			}
		}
	}
}

func TestRelaxedSplitGroupDeletedOwnerRecoversAcrossBoundary(t *testing.T) {
	for _, available := range []bool{false, true} {
		t.Run(map[bool]string{false: "exhausted", true: "available"}[available], func(t *testing.T) {
			h, _, inside, key, c, body := missingOwnerSetup(t, "deleted")
			row := &database.APIKeyRow{ID: 101, AllowedGroupIDs: []int64{10}, Limits: database.APIKeyLimits{NoAffinityGroupIDs: inside.GroupIDSnapshot()}}
			c.Set(contextAPIKeyID, row.ID)
			c.Set(contextAPIKeyRow, row)
			h.store.SetAPIKeyAllowedGroups(row.ID, row.AllowedGroupIDs)
			h.store.SetAPIKeyNoAffinityGroups(row.ID, row.Limits.NoAffinityGroupIDs)
			outside := &auth.Account{DBID: 1697, AccountID: "861373c1-f1a9-4ca9-8682-a0594b30c36c", AccessToken: "outside-token", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}, CodexBPS: true, GroupIDs: []int64{10}, SchedulerPriority: 100}
			h.store.AddAccount(outside)
			if !available {
				inside.Disabled = 1
			}
			body, _ = sjson.SetBytes(body, "reasoning.effort", "high")
			captureGroupRoutingIngress(c, body)
			identity := requestSessionIdentity{stableIdentity: true, affinityID: continuityTestThread, hasRequestFingerprint: true}
			require.Nil(t, h.configureSessionModelAffinity(c, identity, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
			selected, _, handled := h.takeSessionAccountFailover(c.Request.Context(), key, row.ID, nil, applyAffinityGroupRouting(c, identity, nil), auth.DispatchPolicyStandard)
			require.True(t, handled)
			var selectedID int64
			if selected != nil {
				selectedID = selected.ID()
				h.store.Release(selected)
			}
			require.Equal(t, outside.ID(), selectedID, "deleted owners must not preserve a stale split boundary")
			record, _, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
			require.NoError(t, err)
			require.Equal(t, outside.ID(), record.AccountID)
		})
	}
}
