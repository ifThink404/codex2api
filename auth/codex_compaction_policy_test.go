package auth

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNativeCompactionPolicySnapshotKeepsRuntimeAccount(t *testing.T) {
	s := NewStore(nil, nil, nil)
	t.Cleanup(s.Stop)
	dst := &Account{DBID: 51}
	s.AddAccount(dst)
	require.False(t, dst.NativeCompactionOnlyEnabled())
	for _, enabled := range []bool{true, false} {
		s.applyPersistentAccountSnapshot(dst, &Account{DBID: 51, CodexNativeCompactionOnly: enabled}, true)
		require.Same(t, dst, s.FindByID(51))
		require.Equal(t, enabled, dst.NativeCompactionOnlyEnabled())
	}
}
