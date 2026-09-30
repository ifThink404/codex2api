package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestDegradeProbeEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	ctx := context.Background()
	if err := db.MigrateBPSPlugin(ctx); err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	on := true
	store.AddAccount(&auth.Account{DBID: 91, Email: "bps@example.com", AccessToken: "at", Status: auth.StatusReady})
	store.ApplyAccountTransportPluginOverride(91, proxy.BPSPluginID, &on)
	h := &Handler{db: db, store: store, authCacheProxy: proxy.NewHandler(store, db, nil, nil)}
	router := gin.New()
	h.registerTransportPluginRoutes(router.Group("/api/admin"))

	probe := &database.DegradeProbe{AccountID: 91, Route: "bps", Model: "gpt-6-astra", Score: 214, Verdict: "ok", Trigger: "manual", HTML: "<html>pelican</html>"}
	if err := db.InsertDegradeProbe(ctx, probe); err != nil {
		t.Fatal(err)
	}
	rec := doTransportPluginRequest(t, router, http.MethodGet, "/api/admin/plugins/bps/degrade-probes?account_id=91&verdict=ok", "")
	var list struct {
		Probes []struct {
			ID    int64  `json:"id"`
			Name  string `json:"name"`
			HTML  string `json:"html"`
			Score int    `json:"score"`
		} `json:"probes"`
		Threshold int    `json:"threshold"`
		Model     string `json:"model"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list.Probes) != 1 || list.Probes[0].HTML != "" || list.Probes[0].Name != "bps@example.com" || list.Threshold != 187 || list.Model != "gpt-6-astra" {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	rec = doTransportPluginRequest(t, router, http.MethodGet, fmt.Sprintf("/api/admin/plugins/bps/degrade-probes/%d", probe.ID), "")
	var one struct {
		HTML string `json:"html"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &one) != nil || one.HTML != "<html>pelican</html>" {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}
	rec = doTransportPluginRequest(t, router, http.MethodPost, "/api/admin/plugins/bps/degrade-probes", `{"account_ids":[91],"route":"native"}`)
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"queued":0`) || !strings.Contains(rec.Body.String(), "未显式开启原生路由") {
		t.Fatalf("native probe of a BPS account without an explicit native route: %d %s", rec.Code, rec.Body.String())
	}
	rec = doTransportPluginRequest(t, router, http.MethodPost, "/api/admin/plugins/bps/degrade-probes", `{"account_ids":[91,91],"route":"bps"}`)
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"queued":1`) {
		t.Fatalf("bps probe: %d %s", rec.Code, rec.Body.String())
	}
	if h.authCacheProxy.DegradeProbePending(91, "bps") != "queued" {
		t.Fatal("the probe is queued")
	}
	rec = doTransportPluginRequest(t, router, http.MethodPost, "/api/admin/plugins/bps/degrade-probes", `{"account_ids":[91],"route":"bps"}`)
	if !strings.Contains(rec.Body.String(), "已在检测队列中") {
		t.Fatalf("a queued probe is not queued twice: %s", rec.Body.String())
	}
	if rec := doTransportPluginRequest(t, router, http.MethodPost, "/api/admin/plugins/bps/degrade-probes", `{"account_ids":[91],"route":"wifi"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad route: %d", rec.Code)
	}
}
