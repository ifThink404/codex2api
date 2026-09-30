package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestCodexWebSearchProxyLocationSettingPersistsAndApplies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := proxy.CodexWebSearchProxyLocationEnabled()
	t.Cleanup(func() { proxy.SetCodexWebSearchProxyLocation(previous) })
	db := newTestAdminDB(t)
	tc := cache.NewMemory(4)
	t.Cleanup(func() { tc.Close() })
	settings := defaultBootstrapSettings()
	if err := db.UpdateSystemSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, tc, settings)
	t.Cleanup(store.Stop)
	handler := NewHandler(store, db, tc, proxy.NewRateLimiter(settings.GlobalRPM), "test-admin")
	for _, test := range []struct {
		body    string
		enabled bool
	}{
		{`{"codex_web_search_proxy_location":true}`, true},
		{`{"site_name":"Keep web search preference"}`, true},
		{`{"codex_web_search_proxy_location":false}`, false},
	} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(test.body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		handler.UpdateSettings(ctx)
		if recorder.Code != http.StatusOK {
			t.Fatalf("update: %d %s", recorder.Code, recorder.Body.String())
		}
		var response struct {
			Enabled *bool `json:"codex_web_search_proxy_location"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Enabled == nil || *response.Enabled != test.enabled ||
			db.GetCodexWebSearchProxyLocation() != test.enabled || proxy.CodexWebSearchProxyLocationEnabled() != test.enabled {
			t.Fatalf("body %s: response/db/runtime mismatch: %s", test.body, recorder.Body.String())
		}
	}
}

func TestProxyProbeGeoForStorageKeepsOnlyEnglishNames(t *testing.T) {
	result := proxyProbeResult{CountryCode: "US", Region: "加州", City: "洛杉矶"}
	if geo := proxyProbeGeoForStorage(result, "zh-CN"); geo.CountryCode != "US" || geo.Region != "" || geo.City != "" {
		t.Fatalf("localized probe geo = %+v, want country code only", geo)
	}
	result.Region, result.City = "California", "Los Angeles"
	for _, lang := range []string{"", "en", "EN"} {
		if geo := proxyProbeGeoForStorage(result, lang); geo.Region != "California" || geo.City != "Los Angeles" {
			t.Fatalf("lang %q geo = %+v", lang, geo)
		}
	}
}
