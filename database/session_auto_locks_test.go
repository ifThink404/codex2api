package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSessionAutoLocksCRUD(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "locks.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	in := SessionAutoLockInput{SessionKey: "sess-1::api-key:9", SessionIDPrefix: "sess-1", APIKeyID: 9, AccountID: 244, ErrorMessage: "server_is_overloaded", Threshold: 3}
	lock, created, err := db.InsertSessionAutoLock(ctx, in)
	if err != nil || !created || lock == nil || lock.ID == 0 || lock.Source != "automatic" {
		t.Fatalf("insert = %#v created=%v err=%v", lock, created, err)
	}
	again, created, err := db.InsertSessionAutoLock(ctx, in)
	if err != nil || created || again == nil || again.ID != lock.ID {
		t.Fatalf("duplicate insert must be idempotent: %#v created=%v err=%v", again, created, err)
	}
	keys, err := db.ListSessionAutoLockKeys(ctx)
	if err != nil || len(keys) != 1 || keys[0] != in.SessionKey {
		t.Fatalf("keys = %v err=%v", keys, err)
	}
	list, err := db.ListSessionAutoLocks(ctx, 10)
	if err != nil || len(list) != 1 || list[0].AccountID != 244 || list[0].ErrorMessage != "server_is_overloaded" || list[0].Threshold != 3 {
		t.Fatalf("list = %#v err=%v", list, err)
	}
	removed, err := db.DeleteSessionAutoLock(ctx, lock.ID)
	if err != nil || removed == nil || removed.SessionKey != in.SessionKey {
		t.Fatalf("delete = %#v err=%v", removed, err)
	}
	if removed, err := db.DeleteSessionAutoLock(ctx, lock.ID); err != nil || removed != nil {
		t.Fatalf("second delete must be nil,nil: %#v %v", removed, err)
	}
	if keys, _ := db.ListSessionAutoLockKeys(ctx); len(keys) != 0 {
		t.Fatalf("keys after delete = %v", keys)
	}
	long := SessionAutoLockInput{SessionKey: "sess-2", SessionIDPrefix: "sess-2", ErrorMessage: string(make([]byte, 600)), Threshold: 0}
	lock, _, err = db.InsertSessionAutoLock(ctx, long)
	if err != nil || len(lock.ErrorMessage) != 255 || lock.Threshold != 3 {
		t.Fatalf("clamping: len=%d threshold=%d err=%v", len(lock.ErrorMessage), lock.Threshold, err)
	}
}
