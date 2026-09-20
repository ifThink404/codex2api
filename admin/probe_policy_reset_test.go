package admin

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
)

func TestProbePolicyOffDisablesPostResetCallback(t *testing.T) {
	s := auth.NewStore(nil, nil, nil)
	defer s.Stop()
	a := &auth.Account{AccessToken: "token", ProbeMode: "off"}
	var calls atomic.Int32
	h := &Handler{store: s, probeUsage: func(context.Context, *auth.Account) error { calls.Add(1); return nil }}
	if done := h.refreshUsageAfterReset(a); done != nil {
		<-done
	}
	if calls.Load() != 0 {
		t.Fatal("off ran automatic post-reset probe")
	}
}
