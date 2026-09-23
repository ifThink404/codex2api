package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"testing"
	"time"
)

func TestModelCatalogDoesNotDependOnOptionalTransport(t *testing.T) {
	db := newTestModelRegistryDB(t)
	ctx := context.Background()
	id, err := db.InsertAccount(ctx, "test", "rt", "")
	if err != nil {
		t.Fatal(err)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(nil, nil, nil)
	acc := &auth.Account{DBID: id, AccessToken: "token", PlanType: "pro", CredentialGeneration: row.CredentialGeneration, Models: []string{"gpt-6-sol"}}
	store.AddAccount(acc)
	_, err = LearnModelsFromManifest(ctx, db, []byte(`{"models":[{"slug":"gpt-6-sol"}]}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	observation := database.AccountModelObservation{Model: "gpt-6-sol", Source: "manifest", Outcome: "listed", ObservedAt: time.Now().Unix()}
	// This fixture can be written by both the old and new implementation.
	// Legacy transport is injected only into serialized test data.
	raw := []byte(`{"model":"gpt-6-sol","transport":"codex","source":"manifest","outcome":"listed","observed_at":` + fmt.Sprint(observation.ObservedAt) + `}`)
	if err := json.Unmarshal(raw, &observation); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAccountModelObservations(ctx, id, row.CredentialGeneration, []database.AccountModelObservation{observation}); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(store, db, nil, nil)
	for _, enabled := range []bool{false, true, false} {
		store.ApplyAccountCodexBPS(id, enabled)
		if !h.observedCodexManifestModels(ctx, &database.APIKeyRow{ID: 12})["gpt-6-sol"] {
			t.Fatalf("optional transport=%v changed catalog", enabled)
		}
	}
}
