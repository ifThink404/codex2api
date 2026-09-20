package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestProbePolicyEachCredentialEmitsOutboxWithoutLosingOtherCredentials(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "policy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	id, err := db.InsertAccount(ctx, "policy-test", "keep-refresh-token", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []struct {
		key   string
		value interface{}
	}{
		{"probe_mode", "off"}, {"probe_mode", "on"}, {"probe_mode", "auto"},
		{"probe_interval_minutes", 1}, {"probe_interval_minutes", 1440}, {"probe_interval_minutes", 0},
		{"api_auto_recovery_enabled", true}, {"api_auto_recovery_enabled", false},
	} {
		before, err := db.SchedulerOutboxHighWatermark(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = db.UpdateAccountSchedulerMetadata(ctx, id, OptionalNullInt64{}, OptionalNullInt64{}, OptionalBool{}, OptionalInt64Slice{}, OptionalStringSlice{}, OptionalInt64Slice{}, OptionalString{}, map[string]interface{}{change.key: change.value}, AccountPolicyUpdate{})
		if err != nil {
			t.Fatal(err)
		}
		events, err := db.ListSchedulerOutboxEventsAfter(ctx, before, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 1 || events[0].EntityType != SchedulerEntityAccount || events[0].EntityID != id {
			t.Fatalf("%s=%v did not emit account event: %+v", change.key, change.value, events)
		}
		row, err := db.GetAccountByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if row.GetCredential("refresh_token") != "keep-refresh-token" {
			t.Fatal("partial JSON update lost unrelated credential")
		}
	}
}
