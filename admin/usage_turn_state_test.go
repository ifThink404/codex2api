package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// 取值写错必须 400：静默忽略之后页面显示「0 条」，运营会读成「没有这类请求」。
func TestGetUsageLogsRejectsInvalidTurnStateFilters(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name      string
		query     string
		wantError string
	}{
		{"state", "turn_state=maybe", "turn_state 参数无效，需要 received/missing/not_recorded"},
		{"length", "turn_state_length=-1", "turn_state_length 参数无效，需要非负整数"},
		{"length not a number", "turn_state_length=abc", "turn_state_length 参数无效，需要非负整数"},
		{"echo", "turn_state_echo=bogus", "turn_state_echo 参数无效，需要 none/same/cross/unknown/substitute"},
		{"stripped", "turn_state_stripped=1", "turn_state_stripped 参数无效，需要 true 或 false"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := &Handler{}
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(
				http.MethodGet,
				"/api/admin/usage/logs?start=2026-01-01T00:00:00Z&end=2026-01-02T00:00:00Z&page=1&"+test.query,
				nil,
			)
			handler.GetUsageLogs(ctx)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
			}
			assertErrorMessage(t, recorder, test.wantError)
		})
	}
}

func TestGetUsageLogsAppliesTurnStateFilters(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	ctx := context.Background()
	length292, length312, zero := 292, 312, 0
	for _, input := range []*database.UsageLogInput{
		{AccountID: accountID, Endpoint: "/v1/responses", Model: "healthy", StatusCode: http.StatusOK,
			TurnStateLength: &length292, TurnStateEcho: "none"},
		{AccountID: accountID, Endpoint: "/v1/responses", Model: "degraded", StatusCode: http.StatusOK,
			TurnStateLength: &length312, TurnStateEcho: "cross", TurnStateStripped: true},
		{AccountID: accountID, Endpoint: "/v1/responses", Model: "checked-none", StatusCode: http.StatusOK,
			TurnStateLength: &zero, TurnStateEcho: "substitute"},
		{AccountID: accountID, Endpoint: "/v1/responses", Model: "legacy", StatusCode: http.StatusOK},
	} {
		if err := db.InsertUsageLog(ctx, input); err != nil {
			t.Fatalf("InsertUsageLog(%s): %v", input.Model, err)
		}
	}
	db.FlushUsageLogs()

	handler := &Handler{db: db}
	start := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	end := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	cases := []struct {
		query string
		want  string
	}{
		{"turn_state=missing", "checked-none"},
		{"turn_state=not_recorded", "legacy"},
		{"turn_state_length=312", "degraded"},
		{"turn_state_echo=substitute", "checked-none"},
		{"turn_state_stripped=true", "degraded"},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ginCtx, _ := gin.CreateTestContext(recorder)
			ginCtx.Request = httptest.NewRequest(http.MethodGet,
				"/api/admin/usage/logs?start="+start+"&end="+end+"&page=1&page_size=20&"+tc.query, nil)
			handler.GetUsageLogs(ginCtx)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
			}
			var page database.UsageLogPage
			if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if page.Total != 1 || len(page.Logs) != 1 || page.Logs[0].Model != tc.want {
				t.Fatalf("filtered page = total %d logs %+v, want %s only", page.Total, page.Logs, tc.want)
			}
		})
	}

	// received 命中两条，顺带确认 >0 没有把 NULL 行也算进来。
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodGet,
		"/api/admin/usage/logs?start="+start+"&end="+end+"&page=1&page_size=20&turn_state=received", nil)
	handler.GetUsageLogs(ginCtx)
	var page database.UsageLogPage
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.Total != 2 {
		t.Fatalf("turn_state=received total = %d, want 2 (%+v)", page.Total, page.Logs)
	}
}

// 用量日志的 JSON 必须带上三个字段，前端的列和悬浮提示都靠它们。
func TestUsageLogJSONCarriesTurnStateFields(t *testing.T) {
	length := 292
	payload, err := json.Marshal(&database.UsageLog{TurnStateLength: &length, TurnStateEcho: "cross", TurnStateStripped: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["turn_state_length"] != float64(292) || decoded["turn_state_echo"] != "cross" || decoded["turn_state_stripped"] != true {
		t.Fatalf("usage log JSON = %v", decoded)
	}
	payload, err = json.Marshal(&database.UsageLog{})
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal empty: %v", err)
	}
	if value, ok := decoded["turn_state_length"]; !ok || value != nil {
		t.Fatalf("未记录时 turn_state_length 必须是 null，得到 %v (present=%v)", value, ok)
	}
}

// 账号行上的最近 turn-state：按当页账号批量取一次并挂到对应行上。
func TestAttachAccountLatestTurnStates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	ctx := context.Background()
	length292, length312 := 292, 312
	for _, input := range []*database.UsageLogInput{
		{AccountID: accountID, Endpoint: "/v1/responses", StatusCode: http.StatusOK,
			TurnStateLength: &length292, TurnStateEcho: "none"},
		{AccountID: accountID, Endpoint: "/v1/responses", StatusCode: http.StatusOK,
			TurnStateLength: &length312, TurnStateEcho: "cross", TurnStateStripped: true},
	} {
		if err := db.InsertUsageLog(ctx, input); err != nil {
			t.Fatalf("InsertUsageLog: %v", err)
		}
	}
	db.FlushUsageLogs()

	handler := &Handler{db: db}
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts?view=page", nil)

	accounts := []accountResponse{{ID: accountID}, {ID: accountID + 9999}}
	handler.attachAccountLatestTurnStates(context.Background(), accounts)

	latest := accounts[0].LatestTurnState
	if latest == nil || latest.Length == nil || *latest.Length != 312 || latest.Echo != "cross" || !latest.Stripped {
		t.Fatalf("latest turn state = %+v, want the newest row 312/cross/stripped", latest)
	}
	if latest.CreatedAt == "" {
		t.Fatal("latest turn state must carry created_at")
	}
	if accounts[1].LatestTurnState != nil {
		t.Fatalf("账号没有记录时必须整个对象缺席，得到 %+v", accounts[1].LatestTurnState)
	}
}

// 最近 turn-state 只挂在分页视图上：?view=lite 提前返回用的是另一套结构，
// 老的全量视图一次带回整个号池，不该为一个展示字段跑几百路逐账号查找。
func TestListAccountsAttachesLatestTurnStateOnlyForThePagedView(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, codexIDs, _ := newPagedAccountsHandler(t)
	length312 := 312
	if err := handler.db.InsertUsageLog(context.Background(), &database.UsageLogInput{
		AccountID: codexIDs[0], Endpoint: "/v1/responses", StatusCode: http.StatusOK,
		TurnStateLength: &length312, TurnStateEcho: "cross", TurnStateStripped: true,
	}); err != nil {
		t.Fatalf("InsertUsageLog: %v", err)
	}
	handler.db.FlushUsageLogs()

	decode := func(target string) []accountResponse {
		t.Helper()
		recorder := invokeListAccounts(t, handler, target)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s status = %d: %s", target, recorder.Code, recorder.Body.String())
		}
		var paged accountsPageResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &paged); err == nil && len(paged.Accounts) > 0 {
			return paged.Accounts
		}
		var flat []accountResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &flat); err != nil {
			t.Fatalf("%s decode: %v (%s)", target, err, recorder.Body.String())
		}
		return flat
	}
	find := func(accounts []accountResponse, id int64) *accountResponse {
		for i := range accounts {
			if accounts[i].ID == id {
				return &accounts[i]
			}
		}
		return nil
	}

	row := find(decode("/api/admin/accounts?view=page&channel=codex&page=1&page_size=10"), codexIDs[0])
	if row == nil || row.LatestTurnState == nil {
		t.Fatalf("分页视图必须带 latest_turn_state，得到 %+v", row)
	}
	if row.LatestTurnState.Length == nil || *row.LatestTurnState.Length != 312 ||
		row.LatestTurnState.Echo != "cross" || !row.LatestTurnState.Stripped {
		t.Fatalf("latest_turn_state = %+v, want 312/cross/stripped", row.LatestTurnState)
	}

	legacy := find(decode("/api/admin/accounts?channel=codex"), codexIDs[0])
	if legacy == nil {
		t.Fatal("老的全量视图没有返回这个账号，后面的断言会变成空跑")
	}
	if legacy.LatestTurnState != nil {
		t.Fatalf("老的全量视图不该带 latest_turn_state，得到 %+v", legacy.LatestTurnState)
	}
}
