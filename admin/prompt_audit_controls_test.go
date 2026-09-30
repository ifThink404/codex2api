package admin

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestPromptAuditControlsHTTPQuery(t *testing.T) {
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "logs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := &Handler{db: db}
	for _, item := range []database.PromptFilterLogInput{
		{Source: "local_filter", Action: "allow", NewAPIPolicyStatus: "verified", NewAPIPlatform: "test", NewAPIUserID: "1", NewAPIUserName: "alice", AuditScore: 90},
		{Source: "local_filter", Action: "allow", NewAPIPolicyStatus: "verified", NewAPIPlatform: "test", NewAPIUserID: "2", NewAPIUserName: "bob", TextPreview: "alice", AuditScore: 10},
	} {
		if err := db.InsertPromptFilterLog(context.Background(), &item); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		query                string
		status, total, score int
	}{
		{"search_scope=username&q=alice&sort=audit_desc", 200, 1, 90},
		{"search_scope=all&q=alice&sort=audit_asc&page_size=1", 200, 2, 10},
		{"sort=invalid", 400, 0, 0},
		{"search_scope=invalid", 400, 0, 0},
		{"grouped=true&search_scope=username&q=alice", 200, 1, 90},
		{"grouped=invalid", 400, 0, 0},
		{"grouped=", 400, 0, 0},
		{"group_id=", 400, 0, 0},
		{"group_id=0", 400, 0, 0},
		{"group_id=-1", 400, 0, 0},
		{"group_id=9223372036854775808", 400, 0, 0},
	} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest("GET", "/api/prompt-filter/logs?"+tc.query, nil)
		h.ListPromptFilterLogs(c)
		if recorder.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.query, recorder.Code, recorder.Body.String())
		}
		if tc.status != 200 {
			continue
		}
		var result promptFilterLogsResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Total != tc.total || len(result.Logs) != 1 || result.Logs[0].AuditScore != tc.score {
			t.Fatalf("%s: %+v", tc.query, result)
		}
	}
}
