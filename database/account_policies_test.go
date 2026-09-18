package database

import (
	"context"
	"path/filepath"
	"testing"
)

// 三列一次性加到四处 SELECT/Scan（ListActiveByChannel、getAccountByID、
// ListDeleted、ListActiveByIDs）。列表与扫描目标只要错位一格，每次账号加载都会
// 在运行期炸，所以这里把四条读路径全走一遍，而不是只验 GetAccountByID。
func TestAccountPolicyColumnsRoundTrip(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "account-policies.db"))
	if err != nil {
		t.Fatalf("New(sqlite) error: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	id, err := db.InsertAccount(ctx, "policy-account", "rt_policy", "")
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}

	rows, err := db.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListActive returned %d rows, want 1", len(rows))
	}
	if rows[0].PromptFilterPolicy != "inherit" || rows[0].EgressPolicy != "inherit" || rows[0].SessionGuardsPolicy != "inherit" {
		t.Fatalf("defaults must be inherit, got %+v", rows[0])
	}

	projected, err := db.ListActiveByIDs(ctx, []int64{id})
	if err != nil {
		t.Fatalf("ListActiveByIDs: %v", err)
	}
	if len(projected) != 1 {
		t.Fatalf("ListActiveByIDs returned %d rows, want 1", len(projected))
	}
	if projected[0].PromptFilterPolicy != "inherit" || projected[0].EgressPolicy != "inherit" || projected[0].SessionGuardsPolicy != "inherit" {
		t.Fatalf("projection defaults must be inherit, got %+v", projected[0])
	}

	if err := db.UpdateAccountSchedulerMetadata(ctx, id,
		OptionalNullInt64{}, OptionalNullInt64{}, OptionalBool{},
		OptionalInt64Slice{}, OptionalStringSlice{}, OptionalInt64Slice{},
		OptionalString{}, nil,
		AccountPolicyUpdate{
			PromptFilterPolicy:  OptionalString{Set: true, Value: "exempt"},
			EgressPolicy:        OptionalString{Set: true, Value: "direct"},
			SessionGuardsPolicy: OptionalString{Set: true, Value: "off"},
		}); err != nil {
		t.Fatalf("UpdateAccountSchedulerMetadata: %v", err)
	}

	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if row.PromptFilterPolicy != "exempt" || row.EgressPolicy != "direct" || row.SessionGuardsPolicy != "off" {
		t.Fatalf("policies not persisted: %+v", row)
	}

	// 未 Set 的字段不能被覆写回 inherit。
	if err := db.UpdateAccountSchedulerMetadata(ctx, id,
		OptionalNullInt64{}, OptionalNullInt64{}, OptionalBool{},
		OptionalInt64Slice{}, OptionalStringSlice{}, OptionalInt64Slice{},
		OptionalString{}, nil,
		AccountPolicyUpdate{EgressPolicy: OptionalString{Set: true, Value: "inherit"}}); err != nil {
		t.Fatalf("UpdateAccountSchedulerMetadata(partial): %v", err)
	}
	row, err = db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID after partial update: %v", err)
	}
	if row.PromptFilterPolicy != "exempt" || row.EgressPolicy != "inherit" || row.SessionGuardsPolicy != "off" {
		t.Fatalf("partial update must only touch egress_policy: %+v", row)
	}

	// 批量写入走的是另一套 UPDATE 构造器，同样要落库。
	if _, err := db.BatchUpdateAccountMetadata(ctx, []int64{id}, BatchAccountMetadataUpdate{
		Policies: AccountPolicyUpdate{
			PromptFilterPolicy:  OptionalString{Set: true, Value: "inherit"},
			SessionGuardsPolicy: OptionalString{Set: true, Value: "off"},
		},
	}); err != nil {
		t.Fatalf("BatchUpdateAccountMetadata: %v", err)
	}
	row, err = db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID after batch update: %v", err)
	}
	if row.PromptFilterPolicy != "inherit" || row.SessionGuardsPolicy != "off" {
		t.Fatalf("batch update did not persist policies: %+v", row)
	}

	// 回收站列表是第四处 SELECT/Scan，列错位同样只在这里才暴露。
	if err := db.SoftDeleteAccount(ctx, id); err != nil {
		t.Fatalf("SoftDeleteAccount: %v", err)
	}
	deleted, err := db.ListDeleted(ctx)
	if err != nil {
		t.Fatalf("ListDeleted: %v", err)
	}
	if len(deleted) != 1 {
		t.Fatalf("ListDeleted returned %d rows, want 1", len(deleted))
	}
	if deleted[0].SessionGuardsPolicy != "off" || deleted[0].EgressPolicy != "inherit" {
		t.Fatalf("recycle-bin row lost its policies: %+v", deleted[0])
	}
}
