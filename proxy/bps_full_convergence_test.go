package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestBPSFullConvergenceConcurrentTaskCreation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.db")
	first, err := newBPSProxyTestDB("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	second, err := newBPSProxyTestDB("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	account := (&auth.Account{AccountID: accountIdentitySampleAccount}).SetCodexBPSOptions(auth.CodexBPSAccountOptions{Convergence: auth.CodexBPSConvergenceFull})
	type result struct {
		identity *bpsWordIdentityDiagnostic
		err      error
	}
	results := make(chan result, 8)
	for i := range 8 {
		go func() {
			db := []*database.DB{first, second}[i%2]
			ctx := withBPSTestUser(WithCodexIdentityStore(t.Context(), db), fmt.Sprint("user-", i))
			ctx, err := withBPSFullConvergenceTest(ctx, account, bpsProfile(auth.BPSWord), http.Header{}, nil, fmt.Sprint("cache-", i), "key")
			if err != nil {
				results <- result{err: err}
				return
			}
			d, err := resolveBPSWordIdentity(ctx, []byte(`{"input":"hello"}`), http.Header{}, "cache", "", false)
			results <- result{d, err}
		}()
	}
	var task string
	turns := make(map[string]bool)
	for range 8 {
		r := <-results
		require.NoError(t, r.err)
		if task == "" {
			task = r.identity.TaskID
		}
		require.Equal(t, task, r.identity.TaskID)
		require.False(t, turns[r.identity.TurnID])
		turns[r.identity.TurnID] = true
	}
}

func TestBPSFullConvergenceExecutorAndDisabledModes(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	for _, mode := range []string{"account", "preserve", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("CODEX_OUTBOUND_SESSION_MODE", mode)
			db, err := newBPSProxyTestDB("sqlite", filepath.Join(t.TempDir(), "executor.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			a := withBPSOverride((&auth.Account{DBID: 1695, AccountID: accountIdentitySampleAccount, AccessToken: "test-access"}).SetCodexBPSOptions(auth.CodexBPSAccountOptions{Convergence: auth.CodexBPSConvergenceFull}), true)
			var wire []byte
			installClaudeBoundaryTransport(t, a, func(r *http.Request) (*http.Response, error) {
				wire, err = io.ReadAll(r.Body)
				require.NoError(t, err)
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"output":[]}`)), Request: r}, nil
			})
			var task string
			for _, profile := range []auth.CodexBPSProfile{auth.BPSWord, auth.BPSExcel, auth.BPSSheets, auth.BPSPowerPoint} {
				a.SetCodexBPSOptions(auth.CodexBPSAccountOptions{Profile: profile, Convergence: a.CodexBPSConvergence(), ImageTrim: a.CodexBPSImageTrimEnabled()})
				var turn string
				for _, compact := range []bool{false, true} {
					headers, body := accountIdentityFixture(t, false, true)
					c := transportTestContext()
					ctx := WithCodexIdentityStore(c.Request.Context(), db)
					var resp *http.Response
					if compact {
						resp, err = executeBPSTestCompact(ctx, a, body, "cache", "", "test-key", nil, headers)
					} else {
						resp, err = executeBPSTestRequest(ctx, a, body, "cache", "", "test-key", nil, headers, true)
					}
					require.NoError(t, err)
					require.NoError(t, resp.Body.Close())
					if task == "" {
						task = gjson.GetBytes(wire, "metadata.task_id").String()
					}
					require.Equal(t, task, gjson.GetBytes(wire, "metadata.task_id").String())
					if turn == "" {
						turn = gjson.GetBytes(wire, "metadata.turn_id").String()
					}
					require.Equal(t, turn, gjson.GetBytes(wire, "metadata.turn_id").String())
					require.NotNil(t, bpsTraceTransport(ctx).BPS.FullConvergence)
					require.Contains(t, string(wire), "private-encrypted", "full convergence must not merge or remove caller history")
				}
			}
		})
	}
	for _, mode := range []string{auth.CodexFingerprintModeOff, auth.CodexFingerprintModeDevice, auth.CodexBPSConvergenceSession} {
		stale := context.WithValue(t.Context(), bpsFullConvergenceKey{}, &bpsFullConvergenceScope{taskKey: "previous-account"})
		ctx, err := withBPSFullConvergenceTest(stale, (&auth.Account{}).SetCodexBPSOptions(auth.CodexBPSAccountOptions{Convergence: mode}), bpsProfile(auth.BPSWord), nil, nil, "cache", "key")
		require.NoError(t, err)
		require.Nil(t, bpsFullConvergenceFrom(ctx))
	}
	_, err := withBPSFullConvergenceTest(t.Context(), (&auth.Account{}).SetCodexBPSOptions(auth.CodexBPSAccountOptions{Convergence: auth.CodexBPSConvergenceFull}), bpsProfile(auth.BPSWord), nil, nil, "cache", "key")
	require.ErrorContains(t, err, "上游账号 ID")
}
