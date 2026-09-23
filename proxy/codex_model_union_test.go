package proxy

import (
	"context"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"testing"
	"time"
)

func TestCodexManifestExtrasIncludeObservedModelsInKeyScope(t *testing.T) {
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
	acc.SetAllowedAPIKeyIDs([]int64{12})
	store.AddAccount(acc)
	if _, err := LearnModelsFromManifest(ctx, db, []byte(`{"models":[{"slug":"gpt-6-sol"}]}`), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAccountModelObservations(ctx, id, row.CredentialGeneration, []database.AccountModelObservation{{Model: "gpt-6-sol", Source: "manifest", Outcome: "listed", ObservedAt: time.Now().Unix()}}); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(store, db, nil, nil)
	got := h.extraRelayManifestModels(ctx, &database.APIKeyRow{ID: 12})
	found := false
	for _, m := range got {
		found = found || m.ID == "gpt-6-sol"
	}
	if !found {
		t.Fatal("observed account model missing from manifest union")
	}
	for _, m := range h.extraRelayManifestModels(ctx, &database.APIKeyRow{ID: 13}) {
		if m.ID == "gpt-6-sol" {
			t.Fatal("other key's account model leaked")
		}
	}
	acc.Models = []string{"gpt-5.5"}
	for _, m := range h.extraRelayManifestModels(ctx, &database.APIKeyRow{ID: 12}) {
		if m.ID == "gpt-6-sol" {
			t.Fatal("allowlist ignored")
		}
	}
}
