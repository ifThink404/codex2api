package admin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
)

func TestModelProbeIgnoresOptionalRequestTransport(t *testing.T) {
	oldResin := proxy.GetResinConfig()
	oldRuntime := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.SetResinConfig(oldResin); proxy.ApplyRuntimeSettings(oldRuntime) })
	cfg := oldRuntime
	cfg.CodexForceWebsocket = false
	proxy.ApplyRuntimeSettings(cfg)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			http.Error(w, "unexpected optional transport", 502)
			return
		}
		if !strings.Contains(r.URL.Path, "chatgpt.com/backend-api/codex/responses") {
			t.Errorf("unexpected model probe target %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	}))
	defer server.Close()
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "independence-test"})
	db := newTestAdminDB(t)
	ctx := context.Background()
	id := insertTestAccount(t, db)
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(nil, nil, nil)
	account := &auth.Account{DBID: id, AccessToken: "test-token", PlanType: "pro", CredentialGeneration: row.CredentialGeneration, ProxyURL: server.URL}
	store.AddAccount(account)
	h := &Handler{store: store, db: db}
	// Probes stay native-only evidence: they never go through BPS. An account
	// BPS owns (no explicit native route) is skipped instead, since it gets no
	// native traffic, and its existing evidence is kept.
	for _, tc := range []struct {
		enabled, native, probed bool
	}{{enabled: false, probed: true}, {enabled: true, native: true, probed: true}, {enabled: true}} {
		store.ApplyAccountTransportPluginOverride(id, "bps", &tc.enabled)
		native := tc.native
		account.SetCodexBPSOptions(auth.CodexBPSAccountOptions{Native: &native})
		if !tc.native {
			account.SetCodexBPSOptions(auth.CodexBPSAccountOptions{})
		}
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		results := h.runProbeModels(probeCtx, account, []string{"gpt-6-sol"}, 1, nil)
		cancel()
		want := "available"
		if !tc.probed {
			want = "error"
		}
		if len(results) != 1 || results[0].Outcome != want {
			t.Fatalf("bps=%v native=%v: probe = %+v, want %s", tc.enabled, tc.native, results, want)
		}
		evidence, err := db.ListAccountModelObservations(ctx, []int64{id})
		if err != nil {
			t.Fatal(err)
		}
		if len(evidence[id]) != 1 || evidence[id][0].Outcome != "available" {
			t.Fatalf("probe evidence missing or overwritten: %+v", evidence)
		}
	}
}
