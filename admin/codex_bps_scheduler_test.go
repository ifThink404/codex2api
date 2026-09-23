package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

func TestCodexBPSSchedulerUpdatePublishesRuntimeValue(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		raw, _ := json.Marshal(enabled)
		update, err := parseAccountSchedulerUpdate(updateAccountSchedulerReq{CodexBPS: raw})
		if err != nil {
			t.Fatal(err)
		}
		if !update.CodexBPS.Set || update.CodexBPS.Value != enabled {
			t.Fatalf("runtime update missing: %+v", update.CodexBPS)
		}
		if update.CredentialUpdates[auth.CodexBPSEnabledCredentialKey] != enabled {
			t.Fatal("persistence and runtime disagree")
		}
	}
}

func TestCodexBPSSchedulerHTTPUpdatesRuntimeWithoutRestart(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	id := insertTestAccount(t, db)
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	account := &auth.Account{DBID: id, AccessToken: "token", Status: auth.StatusReady}
	store.AddAccount(account)
	h := &Handler{db: db, store: store}
	for _, enabled := range []bool{true, false} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(id, 10)}}
		c.Request = httptest.NewRequest(http.MethodPatch, "/api/admin/accounts/1/scheduler", strings.NewReader(`{"codex_bps_enabled":`+strconv.FormatBool(enabled)+`}`))
		h.UpdateAccountScheduler(c)
		if rec.Code != 200 {
			t.Fatalf("status=%d: %s", rec.Code, rec.Body.String())
		}
		if account.CodexBPSEnabled() != enabled {
			t.Fatal("saved switch not published to active account")
		}
		row, err := db.GetAccountByID(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if row.GetCredentialBool(auth.CodexBPSEnabledCredentialKey) != enabled {
			t.Fatal("switch not persisted")
		}
	}
}
