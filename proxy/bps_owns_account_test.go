package proxy

import (
	"testing"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
)

func TestBPSOwnsAccountKeepsNativeTrafficOffBPSAccounts(t *testing.T) {
	bpsAccount := withBPSOverride(&auth.Account{DBID: 9301, AccountID: "bps", AccessToken: "at"}, true)
	explicitNative := withBPSOverride((&auth.Account{DBID: 9302, AccountID: "native", AccessToken: "at"}).SetCodexBPSOptions(auth.CodexBPSAccountOptions{Native: boolPtrForUpstreamModelTest(true)}), true)
	nativeOnly := &auth.Account{DBID: 9303, AccountID: "plain", AccessToken: "at"}
	relay := &auth.Account{DBID: 9304, AccessToken: "at", UpstreamType: auth.UpstreamOpenAIResponses}

	require.True(t, BPSOwnsAccount(bpsAccount))
	require.False(t, BPSOwnsAccount(explicitNative), "an explicit native route allows native traffic")
	require.False(t, BPSOwnsAccount(nativeOnly))
	require.False(t, BPSOwnsAccount(relay))
	require.False(t, BPSOwnsAccount(withBPSOverride(&auth.Account{DBID: 9305, AccountID: "off", AccessToken: "at"}, false)))

	// Image generation is native only: BPS accounts never take it.
	require.False(t, imageCapableAccountFilter(bpsAccount))
	require.True(t, imageCapableAccountFilter(explicitNative))
	require.True(t, imageCapableAccountFilter(nativeOnly))
}
