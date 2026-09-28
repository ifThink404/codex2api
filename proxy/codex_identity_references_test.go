package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type countingParentIdentityStore struct {
	CodexIdentityStore
	reads map[string]int
}

func (s *countingParentIdentityStore) ReadCodexIdentityReference(ctx context.Context, reference, identity string) (database.CodexIdentityEpoch, bool, bool, error) {
	s.reads[reference]++
	return s.CodexIdentityStore.ReadCodexIdentityReference(ctx, reference, identity)
}

func TestRelaxedMigratedParentReferencesPersistWithoutChangingInput(t *testing.T) {
	for _, preserve := range []bool{false, true} {
		for _, stringMetadata := range []bool{false, true} {
			t.Run(fmt.Sprintf("preserve=%t/string=%t", preserve, stringMetadata), func(t *testing.T) {
				h, owner, target := legacyParentTestSetup(t)
				UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = true; return s })
				current := owner
				for generation, selected := range []*auth.Account{target, owner, target} {
					_, _, err := h.db.SwitchSessionContinuityAccount(t.Context(), database.SessionAccountFailover{
						RootKey: hashRiskIdentity(accountIdentitySampleRoot), ExpectedAccountID: current.ID(), AccountID: selected.ID(),
						ExpectedGeneration: uint64(generation), ResetOutboundWindow: true, WindowThreadID: accountIdentitySampleRoot,
						WindowNumber: uint64(55 + generation), LossyContextRestart: true, PreserveRestartInput: preserve,
					})
					require.NoError(t, err)
					c, body := legacyParentTestRequest(t, h, accountIdentitySampleRoot, continuityTestThread, "turn", uint64(55+generation), stringMetadata)
					body = addSessionTools(t, body)
					body, _ = sjson.SetBytes(body, "client_metadata.x_codex_turn_metadata", fmt.Sprintf(`{"guardian_classifier_source_thread_id":%q}`, continuityTestThread))
					original, ingressHeaders := bytes.Clone(body), c.Request.Header.Clone()
					cleaned, headers, err := PrepareSessionRestartOutbound(c.Request.Context(), selected, body, c.Request.Header)
					require.NoError(t, err)
					store := &countingParentIdentityStore{CodexIdentityStore: h.db, reads: make(map[string]int)}
					ctx := WithCodexIdentityStore(c.Request.Context(), store)
					fingerprint := NewCodexTransportFingerprint(selected, headers, cleaned, "cache", ctx)
					require.NoError(t, fingerprint.ClaimSessionIdentity(ctx, selected, "test-user-key"))
					require.Len(t, fingerprint.accountIdentityDiagnostic.References, 1)
					require.Equal(t, "detached", fingerprint.accountIdentityDiagnostic.References[0].Action)
					out := fingerprint.ApplyBody(cleaned)
					sentHeaders := http.Header{}
					fingerprint.ApplySessionHeaders(sentHeaders)
					require.Empty(t, codexAccountIdentityReferences(sentHeaders, out))
					require.NotContains(t, string(out), continuityTestThread)
					assertSessionTools(t, out)
					require.JSONEq(t, gjson.GetBytes(original, "input").Raw, gjson.GetBytes(out, "input").Raw)
					require.Equal(t, out, fingerprint.ApplyBody(out))
					require.Equal(t, original, body)
					require.Equal(t, ingressHeaders, c.Request.Header)
					for _, count := range store.reads {
						require.Equal(t, 1, count, "each parent is resolved once")
					}
					// A process-local cache reset and switch-off must not resurrect it.
					UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = false; return s })
					h.continuityRecords = nil
					resumed, nextBody := legacyParentTestRequest(t, h, accountIdentitySampleRoot, continuityTestThread, "turn", uint64(55+generation), stringMetadata)
					next := NewCodexTransportFingerprint(selected, resumed.Request.Header, nextBody, "cache", resumed.Request.Context())
					require.NoError(t, next.ClaimSessionIdentity(resumed.Request.Context(), selected, "test-user-key"))
					require.Equal(t, "previously_detached", next.accountIdentityDiagnostic.References[0].Reason)
					require.Equal(t, sentHeaders.Get(codexSessionIDHeader), next.headers.Get(codexSessionIDHeader))
					require.Empty(t, codexAccountIdentityReferences(next.headers, next.ApplyBody(nextBody)))
					UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = true; return s })
					current = selected
				}
				parent, found, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(continuityTestThread))
				require.NoError(t, err)
				require.True(t, found)
				require.Equal(t, owner.ID(), parent.AccountID)
				require.Zero(t, parent.FailoverCount)
			})
		}
	}
}

func TestRelaxedParentReferenceStillRequiresVerifiedRestart(t *testing.T) {
	for _, scenario := range []string{"strict", "not_restarted", "wrong_account", "parent_lookup_failed", "reference_lookup_failed", "missing_parent", "mapped_parent"} {
		t.Run(scenario, func(t *testing.T) {
			h, owner, target := legacyParentTestSetup(t)
			UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings {
				s.CodexForkAccountFallbackEnabled = scenario != "strict"
				return s
			})
			_, _, err := h.db.SwitchSessionContinuityAccount(t.Context(), database.SessionAccountFailover{
				RootKey: hashRiskIdentity(accountIdentitySampleRoot), ExpectedAccountID: owner.ID(), AccountID: target.ID(),
				ResetOutboundWindow: true, WindowThreadID: accountIdentitySampleRoot, WindowNumber: 55, LossyContextRestart: scenario != "not_restarted",
			})
			require.NoError(t, err)
			c, body := legacyParentTestRequest(t, h, accountIdentitySampleRoot, continuityTestThread, "turn", 55, false)
			ctx := c.Request.Context()
			if scenario == "parent_lookup_failed" || scenario == "missing_parent" {
				ctx = context.WithValue(ctx, codexReferenceRootLookupKey{}, codexReferenceRootLookup(func(context.Context, string) (string, database.SessionContinuityRecord, bool, error) {
					if scenario == "missing_parent" {
						return "", database.SessionContinuityRecord{}, false, nil
					}
					return "", database.SessionContinuityRecord{}, false, errors.New("private-db-secret")
				}))
			}
			if scenario == "reference_lookup_failed" {
				ctx = WithCodexIdentityStore(ctx, failedParentReferenceStore{h.db})
			}
			if scenario == "wrong_account" {
				outboundEpochFromContext(ctx).record.AccountID = owner.ID()
			}
			var mappedParent string
			if scenario == "mapped_parent" {
				// A real parent mapped on the target is usable, even if the child migrated.
				_, _, err := h.db.SwitchSessionContinuityAccount(t.Context(), database.SessionAccountFailover{RootKey: hashRiskIdentity(continuityTestThread), ExpectedAccountID: owner.ID(), AccountID: target.ID(), ResetOutboundWindow: true, WindowThreadID: continuityTestThread, WindowNumber: 55})
				require.NoError(t, err)
				parentRequest, parentBody := legacyParentTestRequest(t, h, continuityTestThread, "", "turn", 55, false)
				parent := NewCodexTransportFingerprint(target, parentRequest.Request.Header, parentBody, "cache", parentRequest.Request.Context())
				require.NoError(t, parent.ClaimSessionIdentity(parentRequest.Request.Context(), target, "test-user-key"))
				mappedParent = parent.headers.Get(codexSessionIDHeader)
			}
			fingerprint := NewCodexTransportFingerprint(target, c.Request.Header, body, "cache", ctx)
			attachUpstreamTrace(c, nil)
			ctx = context.WithValue(ctx, upstreamTraceContextKey{}, upstreamTraceFromContext(c.Request.Context()))
			err = fingerprint.ClaimSessionIdentity(ctx, target, "test-user-key")
			if scenario == "missing_parent" || scenario == "mapped_parent" {
				require.NoError(t, err)
				if mappedParent == "" {
					require.Empty(t, codexAccountIdentityReferences(fingerprint.headers, fingerprint.ApplyBody(body)))
				} else {
					require.Equal(t, mappedParent, fingerprint.headers.Get(codexParentThreadIDHeader))
					require.Equal(t, "mapped", fingerprint.accountIdentityDiagnostic.References[0].Action)
				}
			} else {
				require.Error(t, err)
				require.Nil(t, fingerprint.accountIdentity)
				require.NotContains(t, err.Error(), "private-db-secret")
				if strings.HasSuffix(scenario, "lookup_failed") {
					require.Equal(t, scenario, fingerprint.accountIdentityDiagnostic.References[0].Reason)
					claim := snapshotUpstreamTrace(ctx).IdentityClaim
					require.Equal(t, "mapping_failed", claim.Result)
					require.Equal(t, "parent_reference", claim.FailureStage)
					require.Equal(t, scenario, claim.Reason)
				}
			}
		})
	}
}

func TestRelaxedParentFailoverBPSExecutor(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	h, owner, target := legacyParentTestSetup(t)
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = true; return s })
	off := false
	target.CodexBPS, target.CodexNative = true, &off
	_, _, err := h.db.SwitchSessionContinuityAccount(t.Context(), database.SessionAccountFailover{
		RootKey: hashRiskIdentity(accountIdentitySampleRoot), ExpectedAccountID: owner.ID(), AccountID: target.ID(),
		UpstreamMode: "bps", ResetOutboundWindow: true, WindowThreadID: accountIdentitySampleRoot, WindowNumber: 55, LossyContextRestart: true,
	})
	require.NoError(t, err)
	c, body := legacyParentTestRequest(t, h, accountIdentitySampleRoot, continuityTestThread, "turn", 55, false)
	body = addSessionTools(t, body)
	sent := 0
	installClaudeBoundaryTransport(t, target, func(r *http.Request) (*http.Response, error) {
		wire, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NotContains(t, string(wire), continuityTestThread)
		require.Empty(t, r.Header.Get(codexParentThreadIDHeader))
		require.NotEmpty(t, gjson.GetBytes(wire, "metadata.task_id").String())
		assertSessionTools(t, wire)
		sent++
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"output":[]}`)), Request: r}, nil
	})
	response, err := ExecuteRequest(c.Request.Context(), target, body, "cache", "", "test-user-key", nil, c.Request.Header, false)
	require.NoError(t, err)
	require.Equal(t, 200, response.StatusCode)
	require.NoError(t, response.Body.Close())
	require.Equal(t, 1, sent)
}

type failedParentReferenceStore struct{ CodexIdentityStore }

func (s failedParentReferenceStore) ReadCodexIdentityReference(context.Context, string, string) (database.CodexIdentityEpoch, bool, bool, error) {
	return database.CodexIdentityEpoch{}, false, false, errors.New("private-db-secret")
}

func TestRelaxedParentFailoverPreviewAndHTTPExecutor(t *testing.T) {
	for _, compact := range []bool{false, true} {
		t.Run(fmt.Sprint(compact), func(t *testing.T) {
			h, owner, target, key := failoverTestSetup(t, false)
			UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexForkAccountFallbackEnabled = true; return s })
			owner.Status = auth.StatusError
			_, err := h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(accountIdentitySampleRoot), database.SessionContinuityRecord{AccountID: owner.ID(), ThreadID: accountIdentitySampleRoot})
			require.NoError(t, err)
			c, body := failoverTestRequest(t, h)
			body = addSessionTools(t, body)
			body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.parent_thread_id", accountIdentitySampleRoot)
			c.Request.Header.Set(codexParentThreadIDHeader, accountIdentitySampleRoot)
			c.Set(ingressRequestBodyContextKey, body)
			require.Nil(t, h.configureSessionModelAffinity(c, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", compact, body))
			selected, _, handled := h.takeSessionAccountFailover(c.Request.Context(), key, 0, nil, nil, auth.DispatchPolicyStandard)
			require.True(t, handled)
			require.Same(t, target, selected)
			defer h.store.Release(selected)
			root := codexIdentityDigest("codex-account-segment-root-v1", codexIdentityDigest("codex-owner-v1", "credential:test-user-key"), target.EffectiveAccountID(), outboundEpochFromContext(c.Request.Context()).identityKey(), continuityTestThread)
			refKey := codexIdentityDigest("codex-account-reference-v1", root, accountIdentitySampleRoot)
			_, found, bound, err := h.db.ReadCodexIdentityReference(t.Context(), refKey, strings.Repeat("a", 64))
			require.NoError(t, err)
			require.False(t, found, "candidate preview must not commit a detached reference")
			require.False(t, bound)
			previous := GetResinConfig()
			t.Cleanup(func() { SetResinConfig(previous) })
			seen := make(chan []byte, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				out := readUpstreamRequestBody(r)
				if len(codexAccountIdentityReferences(r.Header, out)) != 0 {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				seen <- out
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"response-restarted","output":[]}`))
			}))
			t.Cleanup(server.Close)
			SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "parent-restart"})
			var response *http.Response
			if compact {
				response, err = ExecuteCompactRequest(c.Request.Context(), target, body, "cache", "", "test-user-key", nil, c.Request.Header)
			} else {
				response, err = ExecuteRequest(c.Request.Context(), target, body, "cache", "", "test-user-key", nil, c.Request.Header, false)
			}
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, response.StatusCode)
			require.NoError(t, response.Body.Close())
			out := <-seen
			assertSessionTools(t, out)
			require.JSONEq(t, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(out, "input").Raw)
			ref, found, bound, err := h.db.ReadCodexIdentityReference(t.Context(), refKey, strings.Repeat("a", 64))
			require.NoError(t, err)
			require.True(t, found && bound && ref.Detached)
		})
	}
}

type codexIdentityWithoutPublishedEpochs struct{ CodexIdentityStore }

func (store codexIdentityWithoutPublishedEpochs) PublishCodexIdentityEpoch(context.Context, string, database.CodexIdentityEpoch) error {
	return nil
}

func TestCodexIdentityForkReferenceUsesMappedParentAndStaysFixed(test *testing.T) {
	handler, owner, target, key := failoverTestSetup(test, true)
	mapParent := func(account *auth.Account, record database.SessionContinuityRecord) string {
		request, body := outboundEpochTestRequest(test, handler, 0)
		handler.attachSessionOutboundEpoch(request, hashRiskIdentity(key), record)
		fingerprint := NewCodexTransportFingerprint(account, request.Request.Header, body, "cache")
		require.NoError(test, fingerprint.ClaimSessionIdentity(request.Request.Context(), account, "test-user-key"))
		return gjson.GetBytes(fingerprint.ApplyBody(body), "client_metadata.thread_id").String()
	}
	mapFork := func(account *auth.Account, thread, credential string) (*CodexFingerprint, []byte, error) {
		request, _ := outboundEpochTestRequest(test, handler, 0)
		body := []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","input":"plaintext fork","client_metadata":{"session_id":"%s","thread_id":"%s","x-codex-forked-from-thread-id":"%s","x-codex-turn-metadata":{"session_id":"%s","thread_id":"%s","forked_from_thread_id":"%s","thread_source":"user","request_kind":"turn"}}}`, thread, thread, continuityTestThread, thread, thread, continuityTestThread))
		body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.guardian_classifier_source_thread_id", continuityTestThread)
		headers := http.Header{}
		headers.Set("X-Codex-Forked-From-Thread-Id", continuityTestThread)
		fingerprint := NewCodexTransportFingerprint(account, headers, body, "cache")
		err := fingerprint.ClaimSessionIdentity(request.Request.Context(), account, credential)
		return fingerprint, body, err
	}
	current := owner
	var firstParent, returnedParent, ownerParent string
	for index, account := range []*auth.Account{target, owner, target} {
		record, _, err := handler.db.SwitchSessionContinuityAccount(test.Context(), database.SessionAccountFailover{RootKey: hashRiskIdentity(key), ExpectedAccountID: current.ID(), AccountID: account.ID(), ExpectedGeneration: uint64(index), ResetOutboundWindow: true, WindowThreadID: continuityTestThread})
		require.NoError(test, err)
		mapped := mapParent(account, record)
		require.NotEqual(test, continuityTestThread, mapped)
		current = account
		if index == 0 {
			firstParent = mapped
			fork, body, err := mapFork(target, accountIdentitySampleRoot, "test-user-key")
			require.NoError(test, err)
			require.Equal(test, firstParent, gjson.GetBytes(fork.ApplyBody(body), "client_metadata.x-codex-forked-from-thread-id").String())
		} else if index == 1 {
			ownerParent = mapped
		} else {
			returnedParent = mapped
		}
	}
	require.NotEqual(test, firstParent, returnedParent)
	for _, scenario := range []struct {
		account *auth.Account
		thread  string
		parent  string
	}{
		{target, accountIdentitySampleRoot, firstParent},
		{target, "01a09302-49f4-7b53-b545-91ef29610318", returnedParent},
		{owner, accountIdentitySampleRoot, ownerParent},
	} {
		fork, body, err := mapFork(scenario.account, scenario.thread, "test-user-key")
		require.NoError(test, err)
		mapped := fork.ApplyBody(body)
		require.NotEqual(test, scenario.thread, gjson.GetBytes(mapped, "client_metadata.thread_id").String())
		require.Equal(test, scenario.parent, gjson.GetBytes(mapped, "client_metadata.x-codex-forked-from-thread-id").String())
		require.Equal(test, scenario.parent, gjson.GetBytes(mapped, "client_metadata.x-codex-turn-metadata.forked_from_thread_id").String())
		require.Equal(test, scenario.parent, gjson.GetBytes(mapped, "client_metadata.x-codex-turn-metadata.guardian_classifier_source_thread_id").String())
		outboundHeaders := http.Header{}
		fork.ApplySessionHeaders(outboundHeaders)
		require.Equal(test, scenario.parent, outboundHeaders.Get("X-Codex-Forked-From-Thread-Id"))
		require.Equal(test, mapped, fork.ApplyBody(mapped))
	}
	_, _, err := mapFork(target, accountIdentitySampleRoot, "another-user-key")
	require.Error(test, err)
	settings := CurrentRuntimeSettings()
	settings.CodexSessionFailoverEnabled = false
	ApplyRuntimeSettings(settings)
	fork, body, err := mapFork(target, "01a09302-49f4-7b53-b545-91ef29610319", "test-user-key")
	require.NoError(test, err)
	require.Equal(test, returnedParent, gjson.GetBytes(fork.ApplyBody(body), "client_metadata.x-codex-forked-from-thread-id").String())
	require.NotEqual(test, "01a09302-49f4-7b53-b545-91ef29610319", gjson.GetBytes(fork.ApplyBody(body), "client_metadata.thread_id").String())
}

func TestCodexIdentityForkRecoversMigratedParentFromPreUpgradeRecords(test *testing.T) {
	handler, owner, target, _ := failoverTestSetup(test, true)
	key := hashRiskIdentity(continuityTestThread)
	record, err := handler.db.CommitSessionContinuity(test.Context(), key, database.SessionContinuityRecord{AccountID: owner.ID(), ThreadID: continuityTestThread, NumberKnown: true})
	require.NoError(test, err)
	current := owner
	var originalMapped, currentMapped string
	for index, account := range []*auth.Account{owner, target, owner} {
		if index > 0 {
			record, _, err = handler.db.SwitchSessionContinuityAccount(test.Context(), database.SessionAccountFailover{RootKey: key, ExpectedAccountID: current.ID(), AccountID: account.ID(), ExpectedGeneration: uint64(index - 1), ResetOutboundWindow: true, WindowThreadID: continuityTestThread})
			require.NoError(test, err)
		}
		request, body := outboundEpochTestRequest(test, handler, 0)
		handler.attachSessionOutboundEpoch(request, key, record)
		ctx := WithCodexIdentityStore(request.Request.Context(), codexIdentityWithoutPublishedEpochs{handler.db})
		fingerprint := NewCodexTransportFingerprint(account, request.Request.Header, body, "cache")
		require.NoError(test, fingerprint.ClaimSessionIdentity(ctx, account, "test-user-key"))
		currentMapped = gjson.GetBytes(fingerprint.ApplyBody(body), "client_metadata.thread_id").String()
		if index == 0 {
			originalMapped = currentMapped
		}
		current = account
	}
	require.NotEqual(test, originalMapped, currentMapped)
	settings := CurrentRuntimeSettings()
	settings.CodexSessionFailoverEnabled = false
	ApplyRuntimeSettings(settings)
	request, _ := outboundEpochTestRequest(test, handler, 0)
	body := []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","input":"fork","client_metadata":{"session_id":"%s","thread_id":"%s","x-codex-forked-from-thread-id":"%s","thread_source":"user","request_kind":"turn"}}`, accountIdentitySampleRoot, accountIdentitySampleRoot, continuityTestThread))
	fork := NewCodexTransportFingerprint(owner, nil, body, "cache")
	require.NoError(test, fork.ClaimSessionIdentity(request.Request.Context(), owner, "test-user-key"))
	require.Equal(test, currentMapped, gjson.GetBytes(fork.ApplyBody(body), "client_metadata.x-codex-forked-from-thread-id").String())
}
