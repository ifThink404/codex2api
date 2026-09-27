package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestServiceErrorFilterValidation(test *testing.T) {
	now := time.Now().UTC()
	for _, query := range []string{"start=bad", "status=200", "stage=upstream", "limit=101", "limit=-1", "cursor=invalid", "request_id=" + strings.Repeat("x", 161), "start=2026-09-01T00:00:00Z&end=2026-09-10T00:00:00Z"} {
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodGet, "/?"+query, nil)
		if _, err := parseServiceErrorFilter(ctx, now); err == nil {
			test.Errorf("accepted invalid filter %s", query)
		}
	}
}

func TestServiceErrorAdminList(test *testing.T) {
	db := newTestAdminDB(test)
	db.EnqueueServiceError(database.ServiceErrorEvent{ID: "admin-service-error", StatusCode: 429, Stage: "rate_limit", Message: "concurrency full"})
	deadline := time.Now().Add(5 * time.Second)
	for db.ServiceErrorCollectorStats().Pending > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	handler := &Handler{db: db}
	router := gin.New()
	router.GET("/service-errors", handler.GetServiceErrorLogs)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/service-errors?status=429", nil))
	var page database.ServiceErrorPage
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil || recorder.Code != http.StatusOK || page.Summary.Total != 1 || len(page.Items) != 1 {
		test.Fatalf("list status=%d body=%s err=%v", recorder.Code, recorder.Body.String(), err)
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

func TestServiceErrorGroupedAdminListAndDetails(t *testing.T) {
	db := newTestAdminDB(t)
	for i, id := range []string{"one", "two", "other"} {
		event := database.ServiceErrorEvent{ID: id, RequestID: id, NewAPIIdentityVerified: true, NewAPIUserID: "user-1", StatusCode: 503, Stage: "dispatch", Message: "unavailable"}
		if i == 2 {
			event.NewAPIUserID = "user-2"
		}
		require.True(t, db.EnqueueServiceError(event))
	}
	require.Eventually(t, func() bool { return db.ServiceErrorCollectorStats().Pending == 0 }, 5*time.Second, 10*time.Millisecond)
	handler := &Handler{db: db}
	router := gin.New()
	router.GET("/service-errors", handler.GetServiceErrorLogs)
	get := func(query string, status int) database.ServiceErrorPage {
		t.Helper()
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/service-errors?"+query, nil))
		require.Equal(t, status, recorder.Code, recorder.Body.String())
		var page database.ServiceErrorPage
		if status == 200 {
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &page))
		}
		return page
	}
	page := get("grouped=true", 200)
	require.True(t, page.Grouped)
	require.EqualValues(t, 3, page.Summary.Total)
	require.EqualValues(t, 2, page.Summary.Groups)
	require.Len(t, page.Items, 2)
	for _, event := range page.Items {
		if event.NewAPIUserID == "user-1" {
			details := get("grouped=false&group_key="+event.Group.Key, 200)
			require.Len(t, details.Items, 2)
			require.Nil(t, details.Items[0].Group)
			details = get("group_key="+event.Group.Key+"&request_id=one", 200)
			require.Len(t, details.Items, 1)
		}
	}
	page = get("grouped=true&limit=1", 200)
	require.NotEmpty(t, page.NextCursor)
	get("grouped=false&cursor="+url.QueryEscape(page.NextCursor), 400)
	get("grouped=true&cursor="+url.QueryEscape(page.NextCursor), 200)
	for _, query := range []string{"grouped=maybe", "group_key=invalid", "group_key=%27%20OR%201=1", "grouped=true&group_key=" + strings.Repeat("a", 64)} {
		get(query, 400)
	}
}
