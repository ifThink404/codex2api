package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestAccountModelObservationsVisibleInPageAndDetailWithoutChangingAllowlist(t *testing.T) {
	h, ids, _ := newPagedAccountsHandler(t)
	ctx := context.Background()
	row, err := h.db.GetAccountByID(ctx, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := h.db.SaveAccountModelObservations(ctx, ids[0], row.CredentialGeneration, []database.AccountModelObservation{{Model: "gpt-6-sol", Transport: "codex", Source: "probe", Outcome: "available", ObservedAt: time.Now().Unix()}}); err != nil {
		t.Fatal(err)
	}
	rec := invokeListAccounts(t, h, "/api/admin/accounts?view=page&channel=codex&page_size=20")
	var page accountsPageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	for _, a := range page.Accounts {
		if a.ID == ids[0] && len(a.ModelObservations) != 1 {
			t.Fatal("page missing persisted evidence")
		}
		if a.ID != ids[0] && len(a.ModelObservations) > 0 {
			t.Fatal("account evidence leaked")
		}
	}
	rec = httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(ids[0], 10)}}
	c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/1", nil)
	h.GetAccount(c)
	var a accountResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	if len(a.ModelObservations) != 1 || a.ModelObservations[0].Outcome != "available" {
		t.Fatal("detail missing evidence")
	}
	if len(a.Models) != 0 {
		t.Fatal("observation unexpectedly changed allowlist")
	}
}
