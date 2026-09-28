package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestServiceErrorFilterValidation(test *testing.T) {
	now := time.Now().UTC()
	for _, query := range []string{"start=bad", "status=200", "stage=upstream", "stage=window", "limit=101", "limit=-1", "cursor=invalid", "grouped=maybe", "grouped=true&group_key=" + strings.Repeat("a", 64), "group_key=XYZ", "request_id=" + strings.Repeat("x", 161), "start=2026-09-01T00:00:00Z&end=2026-09-10T00:00:00Z"} {
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodGet, "/?"+query, nil)
		if _, err := parseServiceErrorFilter(ctx, now); err == nil {
			test.Errorf("accepted invalid filter %s", query)
		}
	}
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodGet, "/?status=429&stage=dispatch&grouped=true&limit=50", nil)
	filter, err := parseServiceErrorFilter(ctx, now)
	if err != nil || !filter.Grouped || filter.Limit != 50 || filter.Stage != "dispatch" || !filter.End.Equal(now) {
		test.Fatalf("valid filter = %+v err=%v", filter, err)
	}
}

func TestServiceErrorAdminList(test *testing.T) {
	db := newTestAdminDB(test)
	for _, id := range []string{"one", "two"} {
		db.EnqueueServiceError(database.ServiceErrorEvent{ID: id, RequestID: id, StatusCode: 429, Stage: "rate_limit", Code: "rate_limit_exceeded", Message: "concurrency full"})
	}
	deadline := time.Now().Add(5 * time.Second)
	for db.ServiceErrorCollectorStats().Pending > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	handler := &Handler{db: db}
	router := gin.New()
	router.GET("/service-errors", handler.GetServiceErrorLogs)
	for _, tc := range []struct {
		query string
		items int
	}{{"status=429", 2}, {"status=429&grouped=true", 1}} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/service-errors?"+tc.query, nil))
		var page database.ServiceErrorPage
		if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil || recorder.Code != http.StatusOK || page.Summary.Total != 2 || len(page.Items) != tc.items {
			test.Fatalf("%s: status=%d body=%s err=%v", tc.query, recorder.Code, recorder.Body.String(), err)
		}
		if tc.items == 1 && (page.Items[0].Group == nil || page.Items[0].Group.Count != 2) {
			test.Fatalf("grouped item missing count: %+v", page.Items[0])
		}
	}
}

func TestServiceErrorAdminRouteRequiresAuthentication(test *testing.T) {
	handler := &Handler{db: newTestAdminDB(test)}
	router := gin.New()
	handler.RegisterRoutes(router)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/ops/service-errors", nil))
	if recorder.Code != http.StatusUnauthorized && recorder.Code != http.StatusServiceUnavailable {
		test.Fatalf("unauthenticated access status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
