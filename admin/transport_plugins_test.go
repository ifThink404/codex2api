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
