package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

func TestOpenAIRawPassthroughLifecycle(t *testing.T) {
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, nil)
	defer store.Stop()
	h := &Handler{db: db, store: store}
	for _, initial := range []bool{false, true} {
		enabled := initial
		request := addOpenAIResponsesAccountReq{Name: fmt.Sprintf("raw-%t", initial), BaseURL: "https://relay.example.com", APIKey: fmt.Sprintf("key-%t", initial), Models: []string{"vendor/custom"}}
		if initial {
			request.RawPassthroughEnabled = &enabled
		}
		w := invokeOpenAIResponsesJSONHandler(t, http.MethodPost, "/api/admin/accounts/openai-responses", nil, request, h.AddOpenAIResponsesAccount)
		if w.Code != 200 {
			t.Fatalf("create: %d %s", w.Code, w.Body.String())
		}
		var added struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &added); err != nil {
			t.Fatal(err)
		}
		assert := func(want bool) {
			t.Helper()
			row, err := db.GetAccountByID(context.Background(), added.ID)
			if err != nil {
				t.Fatal(err)
			}
			if row.GetCredentialBool(auth.OpenAIRawPassthroughCredentialKey) != want || store.FindByID(added.ID).OpenAIRawPassthroughEnabled() != want {
				t.Fatalf("persisted/runtime switch mismatch for %d", added.ID)
			}
			reloaded := auth.NewStore(db, nil, nil)
			defer reloaded.Stop()
			if err := reloaded.LoadAccountByID(context.Background(), added.ID); err != nil {
				t.Fatal(err)
			}
			if reloaded.FindByID(added.ID).OpenAIRawPassthroughEnabled() != want {
				t.Fatal("reload lost switch")
			}
		}
		assert(initial)
		request.APIKey = ""
		request.RawPassthroughEnabled = nil
		path := fmt.Sprintf("/api/admin/accounts/%d/openai-responses", added.ID)
		params := gin.Params{{Key: "id", Value: fmt.Sprint(added.ID)}}
		w = invokeOpenAIResponsesJSONHandler(t, http.MethodPut, path, params, request, h.UpdateOpenAIResponsesAccount)
		if w.Code != 200 {
			t.Fatalf("preserve: %d %s", w.Code, w.Body.String())
		}
		assert(initial)
		enabled = !initial
		request.RawPassthroughEnabled = &enabled
		w = invokeOpenAIResponsesJSONHandler(t, http.MethodPut, path, params, request, h.UpdateOpenAIResponsesAccount)
		if w.Code != 200 {
			t.Fatalf("toggle: %d %s", w.Code, w.Body.String())
		}
		assert(!initial)
		list := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(list)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts", nil)
		h.ListAccounts(c)
		var response accountsResponse
		if err := json.Unmarshal(list.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, account := range response.Accounts {
			if account.ID == added.ID {
				found = true
				if account.RawPassthroughEnabled != !initial {
					t.Fatal("list switch mismatch")
				}
			}
		}
		if !found {
			t.Fatal("account missing from list")
		}
	}
}
