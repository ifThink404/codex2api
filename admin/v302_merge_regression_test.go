package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/gin-gonic/gin"
)

func TestV302MergedAdminRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	tc := cache.NewMemory(1)
	t.Cleanup(func() { _ = tc.Close() })
	store := auth.NewStore(db, tc, nil)
	t.Cleanup(store.Stop)
	h := NewHandler(store, db, tc, nil, "merge-test")
	r := gin.New()
	h.RegisterRoutes(r)
	for _, path := range []string{"/api/admin/channel-monitors", "/api/admin/channel-monitors/billing-rates"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("X-Admin-Key", "merge-test")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusOK || !json.Valid(w.Body.Bytes()) {
				t.Fatalf("route failed: status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
	// Check registration without invoking external model probes or version sync.
	want := map[string]bool{
		"GET /api/admin/accounts/:id/model-detector":  false,
		"GET /api/admin/accounts/:id/channel-monitor": false,
		"PUT /api/admin/accounts/:id/channel-monitor": false,
		"POST /api/admin/channel-monitors/:id/probe":  false,
		"POST /api/admin/codex-client-versions/sync":  false,
		"GET /api/admin/session-locks":                false,
	}
	for _, route := range r.Routes() {
		key := route.Method + " " + route.Path
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for route, found := range want {
		if !found {
			t.Errorf("missing merged route: %s", route)
		}
	}
}

func TestV302BPSCapabilitiesRemainIndependent(t *testing.T) {
	for _, req := range []updateAccountSchedulerReq{
		{CodexBPS: json.RawMessage(`true`)},
		{ExcelBPSEnabled: json.RawMessage(`true`)},
		{CodexBPS: json.RawMessage(`false`), ExcelBPSEnabled: json.RawMessage(`true`)},
	} {
		got, err := parseAccountSchedulerUpdate(req)
		if err != nil {
			t.Fatal(err)
		}
		if !got.hasChanges() {
			t.Fatal("capability-only update ignored")
		}
		_, codexSet := got.CredentialUpdates[auth.CodexBPSEnabledCredentialKey]
		_, excelSet := got.CredentialUpdates[auth.ExcelBPSCredentialKey]
		if codexSet != (req.CodexBPS != nil) || excelSet != (req.ExcelBPSEnabled != nil) {
			t.Fatal("BPS transport updates are not independent")
		}
	}
}
