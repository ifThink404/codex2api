package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

func TestProbePolicyAllGETViewsAndRecoveryOptIn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	id, err := db.InsertOpenAIResponsesAccount(context.Background(), "probe views", map[string]interface{}{"upstream_type": auth.UpstreamOpenAIResponses, "api_key": "key", "base_url": "https://example.invalid", "models": []string{"gpt-5.6"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	s := auth.NewStore(db, nil, nil)
	defer s.Stop()
	if err = s.LoadAccountByID(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db, store: s}
	for _, enabled := range []bool{false, true, false} {
		body := fmt.Sprintf(`{"ids":[%d],"api_auto_recovery_enabled":%v,"probe_mode":"off","probe_interval_minutes":7}`, id, enabled)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/batch-update", strings.NewReader(body))
		h.BatchUpdateAccounts(c)
		if w.Code != 200 {
			t.Fatalf("batch status=%d body=%s", w.Code, w.Body.String())
		}
		if got := s.FindByID(id).APIAutoRecoveryEnabledForAccount(); got != enabled {
			t.Fatalf("runtime recovery=%v want %v", got, enabled)
		}
		for _, view := range []string{"", "page", "lite", "detail"} {
			w = httptest.NewRecorder()
			c, _ = gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts?channel=codex&view="+view, nil)
			c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(id)}}
			if view == "detail" {
				h.GetAccount(c)
			} else {
				h.ListAccounts(c)
			}
			if w.Code != 200 {
				t.Fatalf("%s status=%d body=%s", view, w.Code, w.Body.String())
			}
			var payload map[string]any
			if err = json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			var account map[string]any
			if view == "detail" {
				account = payload
				if wrapped, ok := payload["account"].(map[string]any); ok {
					account = wrapped
				}
			} else {
				items, ok := payload["accounts"].([]any)
				if !ok || len(items) != 1 {
					t.Fatalf("%s accounts=%s", view, w.Body.String())
				}
				account = items[0].(map[string]any)
			}
			if account["probe_mode"] != "off" || account["probe_interval_minutes"] != float64(7) || account["api_auto_recovery_enabled"] != enabled {
				t.Fatalf("%s omitted policy=%v", view, account)
			}
		}
	}
}
