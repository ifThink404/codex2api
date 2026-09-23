package database

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestModelObservationsHaveNoTransportContract(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "observations.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	id, err := db.InsertAccount(ctx, "test", "rt", "")
	if err != nil {
		t.Fatal(err)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the old schema readable without allowing its optional-feature rows
	// to become ordinary model evidence after the API is decoupled.
	_, err = db.conn.ExecContext(ctx, `INSERT INTO account_model_observations(account_id,credential_generation,model,transport,source,outcome,observed_at) VALUES($1,$2,'gpt-6-sol','bps','probe','unsupported',200)`, id, row.CredentialGeneration)
	if err != nil {
		t.Fatal(err)
	}
	err = db.SaveAccountModelObservations(ctx, id, row.CredentialGeneration, []AccountModelObservation{{Model: "gpt-6-sol", Source: "probe", Outcome: "available", ObservedAt: 100}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.ListAccountModelObservations(ctx, []int64{id})
	if err != nil {
		t.Fatal(err)
	}
	if len(got[id]) != 1 || got[id][0].Outcome != "available" {
		t.Fatalf("legacy evidence contaminated model observations: %+v", got)
	}
	encoded, err := json.Marshal(got[id])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "transport") {
		t.Fatalf("model contract exposes transport: %s", encoded)
	}
}
