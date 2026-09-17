package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestSessionLocksListAndUnlock(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	store.AddAccount(&auth.Account{DBID: 244, Email: "acct@example.com", AccessToken: "tok"})
	h := &Handler{store: store, db: db}
	lock, _, err := db.InsertSessionAutoLock(context.Background(), database.SessionAutoLockInput{SessionKey: "thread-9::api-key:2", SessionIDPrefix: "thread-9", APIKeyID: 2, AccountID: 244, ErrorMessage: "server_is_overloaded", Threshold: 3})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/session-locks?limit=10", nil)
	h.ListSessionLocks(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Locks []map[string]any `json:"locks"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Total != 1 || len(out.Locks) != 1 {
		t.Fatalf("list body = %s err=%v", rec.Body.String(), err)
	}
	if out.Locks[0]["session_id_prefix"] != "thread-9" || out.Locks[0]["account_name"] != "acct@example.com" || out.Locks[0]["api_key_id"] != float64(2) {
		t.Fatalf("row = %v", out.Locks[0])
	}
	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodDelete, "/api/admin/session-locks/1", nil)
	c.Params = gin.Params{{Key: "id", Value: "1"}}
	h.DeleteSessionLock(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	if removed, err := db.DeleteSessionAutoLock(context.Background(), lock.ID); err != nil || removed != nil {
		t.Fatalf("row must be gone: %#v %v", removed, err)
	}
	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodDelete, "/api/admin/session-locks/1", nil)
	c.Params = gin.Params{{Key: "id", Value: "1"}}
	h.DeleteSessionLock(c)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second delete = %d, want 404", rec.Code)
	}
}
