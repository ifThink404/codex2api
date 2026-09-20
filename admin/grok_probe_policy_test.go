package admin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func TestProbePolicyGrokAutomaticPathsRespectOffAndAutoKey(t *testing.T) {
	for _, mode := range []string{"off", "auto"} {
		t.Run(mode, func(t *testing.T) {
			var generations atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					generations.Add(1)
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"data":[{"id":"grok-4.3"}],"status":"completed","choices":[{"finish_reason":"stop"}],"type":"message","stop_reason":"end_turn"}`)
			}))
			defer server.Close()
			h, db, id := newGrokAdminTestAccount(t, server.URL, false)
			h.store.ApplyAccountProbePolicyPatch(id, map[string]interface{}{auth.ProbeModeCredentialKey: mode})
			a := h.store.FindByID(id)
			if _, err := h.runGrokCapabilityProbe(context.Background(), id, false); err != nil {
				t.Fatal(err)
			}
			h.store.SetGrokProbeConfig(true, 5)
			h.runGrokStatusProbe(context.Background())
			if generations.Load() != 0 {
				t.Fatalf("%s automatically generated %d requests", mode, generations.Load())
			}
			now := time.Now()
			origin, _ := a.GrokCredentials()
			state := &database.GrokAccountState{CredentialGeneration: a.GetCredentialGeneration(), Catalogs: []database.GrokModelCatalog{{Snapshot: database.GrokModelCatalogSnapshot{Origin: origin, CredentialGeneration: a.GetCredentialGeneration(), Status: "ok", ExpiresAt: now.Add(time.Hour)}, Items: []database.GrokModelCatalogItem{{ModelID: "grok-4.3"}}}}}
			if _, err := grokNextMaintenanceDue(a, state, now); err != nil {
				t.Fatalf("disabled optional capabilities caused incomplete control plane: %v", err)
			}
			// Explicit Test remains usable, including off.
			if _, err := h.runGrokCapabilityProbe(context.Background(), id, true); err != nil {
				t.Fatal(err)
			}
			if generations.Load() == 0 {
				t.Fatal("explicit probe was blocked")
			}
			_ = db
		})
	}
}
