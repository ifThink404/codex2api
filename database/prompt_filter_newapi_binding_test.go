package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	promptFilterBindingDDLDriverOnce sync.Once
	promptFilterBindingDDLQueryMu    sync.Mutex
	promptFilterBindingDDLQuery      string
)

type promptFilterBindingDDLDriver struct{}
type promptFilterBindingDDLConn struct{}

func (promptFilterBindingDDLDriver) Open(string) (driver.Conn, error) {
	return promptFilterBindingDDLConn{}, nil
}
func (promptFilterBindingDDLConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("not supported")
}
func (promptFilterBindingDDLConn) Close() error              { return nil }
func (promptFilterBindingDDLConn) Begin() (driver.Tx, error) { return nil, errors.New("not supported") }
func (promptFilterBindingDDLConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	promptFilterBindingDDLQueryMu.Lock()
	promptFilterBindingDDLQuery = query
	promptFilterBindingDDLQueryMu.Unlock()
	return driver.RowsAffected(0), nil
}

func TestPromptFilterNewAPIBindingCRUDAndSecretRotationSQLite(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "bindings.sqlite"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	apiKeyID, err := db.InsertAPIKey(ctx, "fanren", "sk-fanren-binding-test")
	if err != nil {
		t.Fatalf("InsertAPIKey: %v", err)
	}
	otherAPIKeyID, err := db.InsertAPIKey(ctx, "buycodekey", "sk-buycodekey-binding-test")
	if err != nil {
		t.Fatalf("InsertAPIKey other: %v", err)
	}
	binding := &PromptFilterNewAPIBinding{
		APIKeyID: apiKeyID, PlatformCode: "fanren", PlatformName: "凡人 NewAPI",
		Secret: "01234567890123456789012345678901", Enabled: true, RequireSignedIdentity: true,
		PolicyMode: PromptFilterPolicyModeEnforce, PolicyProfile: PromptFilterPolicyProfileBalanced,
	}
	if err := db.CreatePromptFilterNewAPIBinding(ctx, binding); err != nil {
		t.Fatalf("Create binding: %v", err)
	}
	got, err := db.GetPromptFilterNewAPIBinding(ctx, apiKeyID)
	if err != nil {
		t.Fatalf("Get binding: %v", err)
	}
	if got.PlatformCode != "fanren" || got.Secret != binding.Secret || !got.Enabled || !got.RequireSignedIdentity {
		t.Fatalf("binding = %#v", got)
	}
	invalid := *binding
	invalid.APIKeyID = otherAPIKeyID
	invalid.PlatformCode = strings.Repeat("a", 33)
	if err := db.CreatePromptFilterNewAPIBinding(ctx, &invalid); err == nil {
		t.Fatal("overlong platform_code was accepted by database boundary")
	}

	duplicate := *binding
	duplicate.APIKeyID = otherAPIKeyID
	duplicate.Secret = "abcdefghijklmnopqrstuvwxyz123456"
	if err := db.CreatePromptFilterNewAPIBinding(ctx, &duplicate); !errors.Is(err, ErrPromptFilterNewAPIBindingConflict) {
		t.Fatalf("duplicate platform error = %v, want conflict", err)
	}

	got.PlatformCode = "fanren-prod"
	got.PlatformName = "凡人生产站"
	got.PolicyMode = PromptFilterPolicyModeWarn
	got.PolicyProfile = PromptFilterPolicyProfileStrict
	got.Enabled = false
	if err := db.UpdatePromptFilterNewAPIBinding(ctx, got); err != nil {
		t.Fatalf("Update binding: %v", err)
	}

	newSecret := "abcdefghijklmnopqrstuvwxyzABCDEF"
	previousExpiresAt := time.Now().UTC().Add(time.Hour)
	if err := db.ReplacePromptFilterNewAPIBindingSecretAt(ctx, apiKeyID, newSecret, &previousExpiresAt); err != nil {
		t.Fatalf("Replace secret: %v", err)
	}
	rotated, err := db.GetPromptFilterNewAPIBinding(ctx, apiKeyID)
	if err != nil {
		t.Fatalf("Get rotated binding: %v", err)
	}
	if rotated.Secret != newSecret || rotated.PreviousSecret != binding.Secret || rotated.PreviousSecretExpiresAt == nil || !rotated.PreviousSecretExpiresAt.Equal(previousExpiresAt) {
		t.Fatalf("rotated binding = %#v", rotated)
	}
	if err := db.ReplacePromptFilterNewAPIBindingSecret(ctx, apiKeyID, binding.Secret, 0); err != nil {
		t.Fatalf("Replace secret without grace: %v", err)
	}
	rotated, err = db.GetPromptFilterNewAPIBinding(ctx, apiKeyID)
	if err != nil {
		t.Fatalf("Get no-grace binding: %v", err)
	}
	if rotated.PreviousSecret != "" || rotated.PreviousSecretExpiresAt != nil {
		t.Fatalf("previous secret should be cleared: %#v", rotated)
	}
	bindings, err := db.ListPromptFilterNewAPIBindings(ctx)
	if err != nil || len(bindings) != 1 {
		t.Fatalf("List bindings len=%d err=%v", len(bindings), err)
	}
	if err := db.DeletePromptFilterNewAPIBinding(ctx, apiKeyID); err != nil {
		t.Fatalf("Delete binding: %v", err)
	}
	if _, err := db.GetPromptFilterNewAPIBinding(ctx, apiKeyID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Get deleted error = %v, want sql.ErrNoRows", err)
	}
}

func TestNormalizePromptFilterPlatformCode(t *testing.T) {
	valid32 := "A" + strings.Repeat("b", 31)
	if got, ok := NormalizePromptFilterPlatformCode(valid32); !ok || got != strings.ToLower(valid32) {
		t.Fatalf("32-character platform code = %q ok=%v", got, ok)
	}
	for _, value := range []string{
		strings.Repeat("a", 33),
		"_fanren",
		"-fanren",
		"fanren.prod",
		"fanren/prod",
		"fanren:prod",
		"",
	} {
		if got, ok := NormalizePromptFilterPlatformCode(value); ok {
			t.Fatalf("invalid platform code %q normalized to %q", value, got)
		}
	}
}

func TestPromptFilterNewAPIBindingPostgresMigrationDDL(t *testing.T) {
	promptFilterBindingDDLDriverOnce.Do(func() {
		sql.Register("prompt-filter-binding-ddl-capture", promptFilterBindingDDLDriver{})
	})
	conn, err := sql.Open("prompt-filter-binding-ddl-capture", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer conn.Close()
	db := &DB{conn: conn, driver: "postgres"}
	if err := db.ensurePromptFilterNewAPIBindingsTable(context.Background()); err != nil {
		t.Fatalf("ensure postgres table: %v", err)
	}
	promptFilterBindingDDLQueryMu.Lock()
	query := promptFilterBindingDDLQuery
	promptFilterBindingDDLQueryMu.Unlock()
	for _, fragment := range []string{
		"CREATE TABLE IF NOT EXISTS prompt_filter_newapi_bindings",
		"api_key_id INT PRIMARY KEY",
		"platform_code VARCHAR(32) NOT NULL UNIQUE",
		"require_signed_identity BOOLEAN NOT NULL DEFAULT FALSE",
		"previous_secret_expires_at TIMESTAMPTZ NULL",
	} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("postgres DDL missing %q: %s", fragment, query)
		}
	}
}

func TestDeleteAPIKeyDeletesPromptFilterNewAPIBinding(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "delete-binding.sqlite"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	apiKeyID, err := db.InsertAPIKey(ctx, "delete", "sk-delete-binding-test")
	if err != nil {
		t.Fatalf("InsertAPIKey: %v", err)
	}
	if err := db.CreatePromptFilterNewAPIBinding(ctx, &PromptFilterNewAPIBinding{
		APIKeyID: apiKeyID, PlatformCode: "delete-test", PlatformName: "Delete Test",
		Secret: "01234567890123456789012345678901", Enabled: true,
		PolicyMode: PromptFilterPolicyModeInherit, PolicyProfile: PromptFilterPolicyProfileInherit,
	}); err != nil {
		t.Fatalf("Create binding: %v", err)
	}
	if err := db.DeleteAPIKey(ctx, apiKeyID); err != nil {
		t.Fatalf("DeleteAPIKey: %v", err)
	}
	if _, err := db.GetPromptFilterNewAPIBinding(ctx, apiKeyID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("orphan binding error=%v, want sql.ErrNoRows", err)
	}
}
