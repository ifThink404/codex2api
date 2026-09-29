package proxy

import (
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
)

func TestBPSImageTrimDefaultsOnForAccountsWithoutAnOverride(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-6-sol", MaxRetries: 1})
	t.Cleanup(store.Stop)
	unset := &auth.Account{DBID: 9201, AccountID: "unset", AccessToken: "at"}
	store.AddAccount(unset)
	require.True(t, bpsImageTrimEnabled(unset), "fj-server default: history trim on")

	store.ApplyAccountCodexBPSCredentialUpdates(unset.ID(), map[string]any{auth.CodexBPSImageTrimCredentialKey: false})
	require.False(t, bpsImageTrimEnabled(unset), "an explicit value wins")
	store.ApplyAccountCodexBPSCredentialUpdates(unset.ID(), map[string]any{auth.CodexBPSImageTrimCredentialKey: nil})
	require.True(t, bpsImageTrimEnabled(unset), "clearing it inherits the default again")

	off := false
	updateBPSConfig(t, func(c BPSConfig) BPSConfig { c.ImageTrimDefault = &off; return c })
	require.False(t, bpsImageTrimEnabled(unset), "image_trim_default=false turns the default off")
	explicit := (&auth.Account{DBID: 9202, AccountID: "explicit", AccessToken: "at"}).SetCodexBPSOptions(auth.CodexBPSAccountOptions{ImageTrim: true})
	require.True(t, bpsImageTrimEnabled(explicit))
	relay := &auth.Account{DBID: 9203, AccessToken: "at", UpstreamType: auth.UpstreamClaude}
	require.False(t, bpsImageTrimEnabled(relay), "ineligible accounts never trim")
}
