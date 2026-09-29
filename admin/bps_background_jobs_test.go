package admin

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
)

// BPS accounts without an explicit native route never get native inference
// from background jobs.
func TestBackgroundJobsSendNoNativeInferenceForBPSAccounts(t *testing.T) {
	s := auth.NewStore(nil, nil, nil)
	defer s.Stop()
	bps := &auth.Account{DBID: 1, AccessToken: "at", AccountID: "bps", Status: auth.StatusReady}
	bps.SetTransportPluginOverride(proxy.BPSPluginID, true)
	native := &auth.Account{DBID: 2, AccessToken: "at", AccountID: "native", Status: auth.StatusReady}
	native.SetTransportPluginOverride(proxy.BPSPluginID, true)
	native.SetCodexBPSOptions(auth.CodexBPSAccountOptions{Native: func() *bool { v := true; return &v }()})
	s.AddAccount(bps)
	s.AddAccount(native)
	var nativeCalls atomic.Int32
	h := &Handler{store: s, executeUsageProbe: func(context.Context, *auth.Account, []byte, string, string, string, *proxy.DeviceProfileConfig, http.Header, ...bool) (*http.Response, error) {
		nativeCalls.Add(1)
		return nil, errors.New("native probe sent")
	}}
	h.activate5hWindow = func(context.Context, *auth.Account) error {
		nativeCalls.Add(1)
		return nil
	}

	// Usage probe fallback (/responses) and 5h-window activation.
	if err := h.probeUsageViaResponses(context.Background(), bps); !errors.Is(err, errBPSAccountNativeProbe) {
		t.Fatalf("usage probe on a BPS account = %v, want it skipped", err)
	}
	if candidate, activated, err := h.autoActivate5hForAccount(context.Background(), bps, time.Now()); candidate || activated || err != nil {
		t.Fatalf("5h activation on a BPS account = %v %v %v, want skipped", candidate, activated, err)
	}
	// Plan sync after a reset.
	if err := h.syncSingleAccountPlanOnReset(context.Background(), bps); err != nil {
		t.Fatalf("plan sync on a BPS account = %v, want skipped", err)
	}
	if nativeCalls.Load() != 0 {
		t.Fatalf("native inference sent for a BPS account: %d calls", nativeCalls.Load())
	}

	// An explicit native route keeps the native probe.
	if err := h.probeUsageViaResponses(context.Background(), native); err == nil || errors.Is(err, errBPSAccountNativeProbe) {
		t.Fatalf("usage probe on an explicit-native account = %v, want the native probe", err)
	}
	if nativeCalls.Load() != 1 {
		t.Fatalf("native calls = %d, want 1", nativeCalls.Load())
	}
}
