package proxy

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

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
