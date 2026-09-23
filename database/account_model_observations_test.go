package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestAccountModelObservationsAreGenerationAndTransportScoped(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "observations.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	id, err := db.InsertAccount(ctx, "test", "refresh", "")
	if err != nil {
		t.Fatal(err)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	save := func(generation, at int64, transport, outcome string) {
		t.Helper()
		err := db.SaveAccountModelObservations(ctx, id, generation, []AccountModelObservation{{Model: "gpt-6-sol", Transport: transport, Outcome: outcome, Source: "probe", ObservedAt: at}})
		if err != nil {
			t.Fatal(err)
		}
	}
	save(row.CredentialGeneration, 20, "codex", "available")
	save(row.CredentialGeneration, 10, "codex", "unsupported")
	save(row.CredentialGeneration, 30, "bps", "unsupported")
	got, err := db.ListAccountModelObservations(ctx, []int64{id})
	if err != nil {
		t.Fatal(err)
	}
	if len(got[id]) != 2 {
		t.Fatalf("transport observations lost: %+v", got)
	}
	for _, o := range got[id] {
		if o.Transport == "codex" && o.Outcome != "available" {
			t.Fatal("older observation overwrote newer")
		}
	}
	if _, err = db.conn.ExecContext(ctx, `UPDATE accounts SET credential_generation=credential_generation+1 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	save(row.CredentialGeneration, 40, "codex", "available")
	got, err = db.ListAccountModelObservations(ctx, []int64{id})
	if err != nil {
		t.Fatal(err)
	}
	if len(got[id]) != 0 {
		t.Fatal("old credential evidence leaked")
	}
	save(row.CredentialGeneration+1, 50, "codex", "throttled")
	got, err = db.ListAccountModelObservations(ctx, []int64{id})
	if err != nil {
		t.Fatal(err)
	}
	if len(got[id]) != 1 || got[id][0].Outcome != "throttled" {
		t.Fatalf("new generation missing: %+v", got)
	}
}
