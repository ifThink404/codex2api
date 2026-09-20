package auth

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/codex2api/database"
)

// Use a disposable database owned by this test run. Only this test's new row
// is changed; PostgreSQL credentials JSON and the actual outbox are exercised.
func TestPostgresProbePolicyRoundTripReloadAndOutbox(t *testing.T) {
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("CODEX2API_TEST_POSTGRES_DSN is not set")
	}
	db, err := database.New("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	id, err := db.InsertOpenAIResponsesAccount(ctx, "probe-policy-roundtrip", map[string]interface{}{"upstream_type": UpstreamOpenAIResponses, "api_key": "test-only-key", "base_url": "https://example.invalid", "models": []string{"gpt-5.6"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.SoftDeleteAccount(ctx, id)
	s := NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 1, SchedulerEngine: "indexed"})
	defer s.Stop()
	if err = s.LoadAccountByID(ctx, id); err != nil {
		t.Fatal(err)
	}
	a := s.FindByID(id)
	if mode, minutes := a.GetProbePolicy(); mode != "auto" || minutes != 0 || a.APIAutoRecoveryEnabledForAccount() || a.NeedsUsageProbe(time.Minute) {
		t.Fatal("absent settings did not preserve default auto/no probes/no recovery")
	}
	for _, tc := range []struct {
		mode     string
		minutes  int
		recovery bool
	}{{"on", 1, true}, {"off", 1440, false}, {"auto", 0, false}} {
		watermark, err := db.SchedulerOutboxHighWatermark(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = db.UpdateAccountSchedulerMetadata(ctx, id, database.OptionalNullInt64{}, database.OptionalNullInt64{}, database.OptionalBool{}, database.OptionalInt64Slice{}, database.OptionalStringSlice{}, database.OptionalInt64Slice{}, database.OptionalString{}, map[string]interface{}{ProbeModeCredentialKey: tc.mode, ProbeIntervalCredentialKey: tc.minutes, APIAutoRecoveryCredentialKey: tc.recovery}, database.AccountPolicyUpdate{}); err != nil {
			t.Fatal(err)
		}
		row, err := db.GetAccountByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if row.GetCredential(ProbeModeCredentialKey) != tc.mode || ProbeIntervalMinutesFromRow(row) != tc.minutes || row.GetCredentialBool(APIAutoRecoveryCredentialKey) != tc.recovery || row.GetCredential("api_key") != "test-only-key" {
			t.Fatal("credential JSON roundtrip failed or unrelated credential lost")
		}
		events, err := db.ListSchedulerOutboxEventsAfter(ctx, watermark, 100)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, event := range events {
			if event.EntityType == "account" && event.EntityID == id {
				found = true
			}
		}
		if !found {
			t.Fatal("credential update did not emit PostgreSQL outbox event")
		}
		a.mu.Lock()
		a.lastAutomaticProbeAt = time.Now().Add(-time.Minute)
		a.mu.Unlock()
		if err = s.applySchedulerOutboxBatch(ctx, events); err != nil {
			t.Fatal(err)
		}
		if s.FindByID(id) != a {
			t.Fatal("outbox replaced live account pointer")
		}
		if mode, minutes := a.GetProbePolicy(); mode != tc.mode || minutes != tc.minutes || a.APIAutoRecoveryEnabledForAccount() != tc.recovery {
			t.Fatalf("outbox policy=%s/%d/%v", mode, minutes, a.APIAutoRecoveryEnabledForAccount())
		}
		reloaded := NewStore(db, nil, nil)
		if err = reloaded.LoadAccountByID(ctx, id); err != nil {
			reloaded.Stop()
			t.Fatal(err)
		}
		fresh := reloaded.FindByID(id)
		if mode, minutes := fresh.GetProbePolicy(); mode != tc.mode || minutes != tc.minutes || fresh.APIAutoRecoveryEnabledForAccount() != tc.recovery {
			reloaded.Stop()
			t.Fatal("restart/reload lost policy")
		}
		reloaded.Stop()
	}
}
