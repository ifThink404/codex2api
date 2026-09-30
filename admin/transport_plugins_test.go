package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy/plugins"
	"github.com/gin-gonic/gin"
)

type adminTestPlugin struct{}

func (adminTestPlugin) ID() string { return "adminplug" }
func (adminTestPlugin) Describe() plugins.Meta {
	return plugins.Meta{Name: "admin test", Kinds: []plugins.RequestKind{plugins.KindResponses}}
}
func (adminTestPlugin) Admissible(context.Context, *auth.Account, string) (bool, string) {
	return true, ""
}
func (adminTestPlugin) Select(context.Context, plugins.Attempt) bool { return true }
func (adminTestPlugin) Execute(context.Context, *plugins.ReqEnv) (*http.Response, error) {
	return nil, nil
}

func newTransportPluginAdminRouter(t *testing.T) (*gin.Engine, *database.DB, *auth.Store, int64) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	registry := plugins.NewRegistry()
	registry.Register(adminTestPlugin{})
	if err := registry.Attach(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	previous := plugins.SwapDefault(registry)
	t.Cleanup(func() {
		plugins.SwapDefault(previous)
		auth.SetTransportPluginReloader(nil)
	})
	id := insertTestAccount(t, db)
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: id, AccessToken: "token", Status: auth.StatusReady})
	h := &Handler{db: db, store: store}
	router := gin.New()
	h.registerTransportPluginRoutes(router.Group("/api/admin"))
	return router, db, store, id
}

func doTransportPluginRequest(t *testing.T, router *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	return rec
}

func TestTransportPluginAdminStateAndOverrides(t *testing.T) {
	router, db, store, accountID := newTransportPluginAdminRouter(t)

	rec := doTransportPluginRequest(t, router, http.MethodGet, "/api/admin/plugins", "")
	var list struct {
		Plugins []transportPluginResponse `json:"plugins"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list.Plugins) != 1 {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	if p := list.Plugins[0]; p.ID != "adminplug" || p.State.Enabled || p.OverrideCredentialKey != "transport_plugin_adminplug_enabled" {
		t.Fatalf("default plugin state = %+v", p)
	}

	rec = doTransportPluginRequest(t, router, http.MethodPut, "/api/admin/plugins/adminplug", `{"enabled":true,"group_ids":[4],"config":{"k":"v"},"capture_enabled":true,"capture_sample_rate":0.2}`)
	var updated transportPluginResponse
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &updated) != nil {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	if s := updated.State; !s.Enabled || len(s.GroupIDs) != 1 || string(s.Config) != `{"k":"v"}` || !s.CaptureEnabled || s.CaptureSampleRate != 0.2 {
		t.Fatalf("updated state = %+v", s)
	}
	// Partial update keeps the other fields.
	rec = doTransportPluginRequest(t, router, http.MethodPut, "/api/admin/plugins/adminplug", `{"enabled":false}`)
	if rec.Code != 200 || plugins.Default().State("adminplug").CaptureSampleRate != 0.2 || plugins.Default().State("adminplug").Enabled {
		t.Fatalf("partial update: %d %s", rec.Code, rec.Body.String())
	}
	for _, bad := range []string{`{"capture_sample_rate":2}`, `{"config":[1]}`, `not json`} {
		if rec := doTransportPluginRequest(t, router, http.MethodPut, "/api/admin/plugins/adminplug", bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("bad update %s: %d", bad, rec.Code)
		}
	}
	if rec := doTransportPluginRequest(t, router, http.MethodGet, "/api/admin/plugins/nope", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown plugin: %d", rec.Code)
	}

	path := "/api/admin/plugins/adminplug/accounts/" + strconv.FormatInt(accountID, 10)
	if rec := doTransportPluginRequest(t, router, http.MethodPut, path, `{"enabled":true}`); rec.Code != 200 {
		t.Fatalf("override: %d %s", rec.Code, rec.Body.String())
	}
	if enabled, ok := store.FindByID(accountID).TransportPluginOverride("adminplug"); !ok || !enabled {
		t.Fatal("override not applied to runtime account")
	}
	row, err := db.GetAccountByID(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if v := row.GetCredentialOptionalBool("transport_plugin_adminplug_enabled"); v == nil || !*v {
		t.Fatal("override not persisted")
	}
	rec = doTransportPluginRequest(t, router, http.MethodGet, "/api/admin/plugins/adminplug", "")
	var detail transportPluginResponse
	if json.Unmarshal(rec.Body.Bytes(), &detail) != nil || len(detail.Overrides) != 1 || detail.Overrides[0].AccountID != accountID {
		t.Fatalf("detail overrides: %s", rec.Body.String())
	}
	if rec := doTransportPluginRequest(t, router, http.MethodPut, path, `{"enabled":null}`); rec.Code != 200 {
		t.Fatalf("clear override: %d", rec.Code)
	}
	if _, ok := store.FindByID(accountID).TransportPluginOverride("adminplug"); ok {
		t.Fatal("override not cleared")
	}
	if rec := doTransportPluginRequest(t, router, http.MethodPut, "/api/admin/plugins/adminplug/accounts/999999", `{"enabled":true}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown account: %d", rec.Code)
	}
}

func TestTransportPluginAdminCaptures(t *testing.T) {
	router, db, _, _ := newTransportPluginAdminRouter(t)
	now := time.Now()
	err := db.InsertPluginCaptures(context.Background(), []database.PluginCapture{
		{Plugin: "adminplug", RequestID: "r1", AccountID: 3, Direction: "request", Body: "req-body", CreatedAt: now},
		{Plugin: "adminplug", RequestID: "r1", AccountID: 3, Direction: "response", Status: 500, Body: "resp-body", CreatedAt: now},
		{Plugin: "otherplug", RequestID: "r1", Direction: "request", Body: "other", CreatedAt: now},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := doTransportPluginRequest(t, router, http.MethodGet, "/api/admin/plugins/adminplug/captures?request_id=r1&account_id=3&status=500&start="+now.Add(-time.Hour).UTC().Format(time.RFC3339), "")
	var page database.PluginCapturePage
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &page) != nil || page.Total != 1 || page.Captures[0].Body != "" {
		t.Fatalf("captures: %d %s", rec.Code, rec.Body.String())
	}
	id := strconv.FormatInt(page.Captures[0].ID, 10)
	rec = doTransportPluginRequest(t, router, http.MethodGet, "/api/admin/plugins/adminplug/captures/"+id, "")
	var capture database.PluginCapture
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &capture) != nil || capture.Body != "resp-body" {
		t.Fatalf("capture: %d %s", rec.Code, rec.Body.String())
	}
	all, _ := db.ListPluginCaptures(context.Background(), database.PluginCaptureFilter{Plugin: "otherplug"})
	otherID := strconv.FormatInt(all.Captures[0].ID, 10)
	if rec := doTransportPluginRequest(t, router, http.MethodGet, "/api/admin/plugins/adminplug/captures/"+otherID, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-plugin capture read: %d", rec.Code)
	}
	for _, bad := range []string{"?status=x", "?account_id=-1", "?start=yesterday"} {
		if rec := doTransportPluginRequest(t, router, http.MethodGet, "/api/admin/plugins/adminplug/captures"+bad, ""); rec.Code != http.StatusBadRequest {
			t.Fatalf("bad filter %s: %d", bad, rec.Code)
		}
	}
}

func TestUsageLogsFilterParsesTransport(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/usage/logs?transport=adminplug", nil)
	filter, ok := parseUsageLogsFilter(c, time.Now().Add(-time.Hour), time.Now())
	if !ok || filter.Transport != "adminplug" {
		t.Fatalf("filter = %+v ok=%v", filter, ok)
	}
}

func TestOpsErrorsFilterParsesTransport(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/ops/errors?transport=bps", nil)
	filter, ok := parseOpsErrorLogFilter(c, true)
	if !ok || filter.Transport != "bps" || !filter.ErrorOnly {
		t.Fatalf("filter = %+v ok=%v", filter, ok)
	}
}

func TestOpsErrorsFilterParsesRetryAndTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/ops/errors?retry=true&timeout=true&account_id=7", nil)
	filter, ok := parseOpsErrorLogFilter(c, true)
	if !ok || filter.RetryOnly == nil || !*filter.RetryOnly || !filter.TimeoutOnly || filter.AccountID == nil || *filter.AccountID != 7 {
		t.Fatalf("filter = %+v ok=%v", filter, ok)
	}
	rec := httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/ops/errors?retry=maybe", nil)
	if _, ok := parseOpsErrorLogFilter(c, true); ok || rec.Code != http.StatusBadRequest {
		t.Fatalf("bad retry value: ok=%v code=%d", ok, rec.Code)
	}
}

func TestOpsErrorsByAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	db.SetUsageLogConfig(database.UsageLogModeFull, 100, 300)
	ctx := context.Background()
	for _, row := range []database.UsageLogInput{
		{AccountID: 11, Transport: "bps", StatusCode: 429, UpstreamErrorKind: "bps_rate_limited"},
		{AccountID: 11, Transport: "bps", StatusCode: 429, UpstreamErrorKind: "bps_rate_limited"},
		{AccountID: 11, Transport: "bps", StatusCode: 403, UpstreamErrorKind: "bps_policy_blocked"},
		{AccountID: 12, Transport: "bps", StatusCode: 502},
		{AccountID: 13, Transport: "native", StatusCode: 500, UpstreamErrorKind: "server_error"},
		{AccountID: 12, Transport: "bps", StatusCode: 200},
	} {
		input := row
		if err := db.InsertUsageLog(ctx, &input); err != nil {
			t.Fatal(err)
		}
	}
	db.FlushUsageLogs()
	h := &Handler{db: db}
	router := gin.New()
	router.GET("/api/admin/ops/errors/by-account", h.GetOpsErrorsByAccount)
	rec := doTransportPluginRequest(t, router, http.MethodGet, "/api/admin/ops/errors/by-account?transport=bps", "")
	var out struct {
		Accounts []database.UsageErrorAccountGroup `json:"accounts"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("by-account: %d %s", rec.Code, rec.Body.String())
	}
	if len(out.Accounts) != 2 || out.Accounts[0].AccountID != 11 || out.Accounts[0].Total != 3 ||
		out.Accounts[0].Kinds["bps_rate_limited"] != 2 || out.Accounts[0].Kinds["bps_policy_blocked"] != 1 ||
		out.Accounts[1].AccountID != 12 || out.Accounts[1].Kinds["server_error"] != 1 {
		t.Fatalf("accounts = %+v (BPS errors only, most first; a kindless 502 is a server_error)", out.Accounts)
	}
}

func TestTransportPluginAdminAccountStatus(t *testing.T) {
	router, _, _, accountID := newTransportPluginAdminRouter(t)
	id := strconv.FormatInt(accountID, 10)
	rec := doTransportPluginRequest(t, router, http.MethodGet, "/api/admin/plugins/adminplug/account-status?ids="+id, "")
	var out struct {
		Accounts []json.RawMessage `json:"accounts"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Accounts == nil || len(out.Accounts) != 0 {
		t.Fatalf("non-BPS plugin account status: %d %s", rec.Code, rec.Body.String())
	}
	for _, bad := range []string{"?ids=x", "?ids=0", "?ids=" + strings.Repeat("1,", 201)} {
		if rec := doTransportPluginRequest(t, router, http.MethodGet, "/api/admin/plugins/adminplug/account-status"+bad, ""); rec.Code != http.StatusBadRequest {
			t.Fatalf("bad ids %s: %d", bad[:min(len(bad), 20)], rec.Code)
		}
	}
	if rec := doTransportPluginRequest(t, router, http.MethodGet, "/api/admin/plugins/missing/account-status?ids=1", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown plugin: %d", rec.Code)
	}
}

func TestTransportPluginCapturePurgeModesAndStats(t *testing.T) {
	router, db, _, _ := newTransportPluginAdminRouter(t)
	now := time.Now()
	seed := func() {
		t.Helper()
		for _, plugin := range []string{"adminplug", "otherplug"} {
			if _, err := db.PurgePluginCapturesByMode(context.Background(), plugin, database.PluginCapturePurgeAll, time.Time{}); err != nil {
				t.Fatal(err)
			}
		}
		err := db.InsertPluginCaptures(context.Background(), []database.PluginCapture{
			{Plugin: "adminplug", RequestID: "ok-new", Direction: "request", Body: "aaaa", CreatedAt: now},
			{Plugin: "adminplug", RequestID: "ok-old", Direction: "request", Body: "bb", CreatedAt: now.Add(-5 * time.Hour)},
			{Plugin: "adminplug", RequestID: "err-new", Direction: "response", Status: 403, Body: "c", CreatedAt: now},
			{Plugin: "adminplug", RequestID: "classified", Direction: "response", Status: 200, ErrorKind: "bps_policy_blocked", CreatedAt: now},
			{Plugin: "otherplug", RequestID: "other", Direction: "request", CreatedAt: now.Add(-5 * time.Hour)},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	stats := func() database.PluginCaptureStats {
		t.Helper()
		rec := doTransportPluginRequest(t, router, http.MethodGet, "/api/admin/plugins/adminplug/capture-stats", "")
		var out database.PluginCaptureStats
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("stats: %d %s", rec.Code, rec.Body.String())
		}
		return out
	}
	seed()
	if got := stats(); got.Rows != 4 || got.ErrorRows != 2 || got.BodyBytes < 7 {
		t.Fatalf("stats = %+v", got)
	}
	for _, tc := range []struct {
		body    string
		deleted int64
		left    int64
	}{
		{body: `{"mode":"errors_only"}`, deleted: 2, left: 2},
		{body: `{"mode":"older_than","hours":2}`, deleted: 1, left: 3},
		{body: `{"mode":"all"}`, deleted: 4, left: 0},
	} {
		seed()
		rec := doTransportPluginRequest(t, router, http.MethodPost, "/api/admin/plugins/adminplug/captures/purge", tc.body)
		var result database.PluginCapturePurgeResult
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &result) != nil || result.Deleted != tc.deleted {
			t.Fatalf("purge %s: %d %s", tc.body, rec.Code, rec.Body.String())
		}
		if got := stats(); got.Rows != tc.left {
			t.Fatalf("after %s: rows = %d, want %d", tc.body, got.Rows, tc.left)
		}
		other, _ := db.ListPluginCaptures(context.Background(), database.PluginCaptureFilter{Plugin: "otherplug"})
		if other.Total != 1 {
			t.Fatalf("purge %s touched another plugin's captures", tc.body)
		}
	}
	for _, bad := range []string{`{"mode":"everything"}`, `{"mode":"older_than"}`, `{"mode":"older_than","hours":1000}`, `not json`} {
		if rec := doTransportPluginRequest(t, router, http.MethodPost, "/api/admin/plugins/adminplug/captures/purge", bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("bad purge %s: %d", bad, rec.Code)
		}
	}
	if rec := doTransportPluginRequest(t, router, http.MethodPost, "/api/admin/plugins/missing/captures/purge", `{"mode":"all"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown plugin purge: %d", rec.Code)
	}
}

func TestTransportPluginCaptureAdminRoutesRequireAuthorization(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	tokenCache := cache.NewMemory(4)
	t.Cleanup(func() { _ = tokenCache.Close() })
	store := auth.NewStore(db, tokenCache, nil)
	t.Cleanup(store.Stop)
	handler := NewHandler(store, db, tokenCache, nil, "admin-secret")
	router := gin.New()
	handler.RegisterRoutes(router)
	for _, route := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/admin/plugins/bps/captures/purge", `{"mode":"all"}`},
		{http.MethodGet, "/api/admin/plugins/bps/capture-stats", ""},
	} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(route.method, route.path, strings.NewReader(route.body)))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without admin credentials: %d", route.method, route.path, rec.Code)
		}
	}
}

func TestBPSPolicyBlocksEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	if err := db.MigrateBPSPlugin(context.Background()); err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: 41, Email: "blocked@example.com", AccessToken: "at", Status: auth.StatusReady})
	h := &Handler{db: db, store: store}
	router := gin.New()
	h.registerTransportPluginRoutes(router.Group("/api/admin"))
	ctx := context.Background()
	now := time.Now()
	if err := db.OpenBPSPolicyBlock(ctx, 41, now.Add(-90*time.Minute), 2); err != nil {
		t.Fatal(err)
	}
	if err := db.OpenBPSPolicyBlock(ctx, 42, now.Add(-5*time.Hour), 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ClearBPSPolicyBlock(ctx, 42, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	rec := doTransportPluginRequest(t, router, http.MethodGet, "/api/admin/plugins/bps/policy-blocks", "")
	var out struct {
		Active []struct {
			AccountID      int64  `json:"account_id"`
			Name           string `json:"name"`
			Tier           int    `json:"tier"`
			ElapsedSeconds int64  `json:"elapsed_seconds"`
		} `json:"active"`
		History []struct {
			AccountID       int64 `json:"account_id"`
			DurationSeconds int64 `json:"duration_seconds"`
			ElapsedSeconds  int64 `json:"elapsed_seconds"`
		} `json:"history"`
		Totals []struct {
			AccountID           int64 `json:"account_id"`
			TimesBlocked        int   `json:"times_blocked"`
			TotalBlockedSeconds int64 `json:"total_blocked_seconds"`
			LongestBlockSeconds int64 `json:"longest_block_seconds"`
		} `json:"totals"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("policy blocks: %d %s", rec.Code, rec.Body.String())
	}
	if len(out.Active) != 1 || out.Active[0].AccountID != 41 || out.Active[0].Name != "blocked@example.com" || out.Active[0].Tier != 2 {
		t.Fatalf("active = %+v", out.Active)
	}
	if elapsed := out.Active[0].ElapsedSeconds; elapsed < 5395 || elapsed > 5410 {
		t.Fatalf("active elapsed = %ds, want ~5400", elapsed)
	}
	if len(out.History) != 1 || out.History[0].DurationSeconds != 4*3600 || out.History[0].ElapsedSeconds != 4*3600 {
		t.Fatalf("history = %+v", out.History)
	}
	if len(out.Totals) != 2 || out.Totals[1].AccountID != 42 || out.Totals[1].TotalBlockedSeconds != 4*3600 || out.Totals[1].LongestBlockSeconds != 4*3600 || out.Totals[0].TimesBlocked != 1 {
		t.Fatalf("totals = %+v", out.Totals)
	}
	if rec := doTransportPluginRequest(t, router, http.MethodGet, "/api/admin/plugins/missing/policy-blocks", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown plugin: %d", rec.Code)
	}
}
