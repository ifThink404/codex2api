package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestCodexBPSSchedulerFieldsParse(t *testing.T) {
	update, err := parseAccountSchedulerUpdate(updateAccountSchedulerReq{codexBPSAccountFieldsReq: codexBPSAccountFieldsReq{
		Enabled:     json.RawMessage(`null`),
		Native:      json.RawMessage(`true`),
		BPSModels:   json.RawMessage(`["gpt-6-*"," gpt-5.6-sol "]`),
		ImageTrim:   json.RawMessage(`true`),
		Profile:     json.RawMessage(`"excel"`),
		Convergence: json.RawMessage(`"turn_round"`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	creds := update.CredentialUpdates
	if value, ok := creds[auth.CodexBPSEnabledCredentialKey]; !ok || value != nil {
		t.Fatalf("null override must clear the key, got %#v", value)
	}
	if creds[auth.CodexNativeEnabledCredentialKey] != true || creds[auth.CodexBPSImageTrimCredentialKey] != true ||
		creds[auth.CodexBPSProfileCredentialKey] != "excel" || creds[auth.CodexBPSConvergenceCredentialKey] != "turn_round" {
		t.Fatalf("credential updates = %#v", creds)
	}
	if models, _ := creds[auth.CodexBPSModelsCredentialKey].([]string); len(models) != 2 || models[1] != "gpt-5.6-sol" {
		t.Fatalf("bps models = %#v", creds[auth.CodexBPSModelsCredentialKey])
	}
	for name, req := range map[string]codexBPSAccountFieldsReq{
		"profile":     {Profile: json.RawMessage(`"visio"`)},
		"convergence": {Convergence: json.RawMessage(`"device"`)},
		"models":      {BPSModels: json.RawMessage(`["gpt*x"]`)},
		"enabled":     {Enabled: json.RawMessage(`"yes"`)},
	} {
		if _, err := parseAccountSchedulerUpdate(updateAccountSchedulerReq{codexBPSAccountFieldsReq: req}); err == nil {
			t.Fatalf("%s: invalid value accepted", name)
		}
	}
}

func TestCodexBPSSchedulerHTTPUpdatesRuntimeWithoutRestart(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	id := insertTestAccount(t, db)
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	account := &auth.Account{DBID: id, AccessToken: "token", Status: auth.StatusReady}
	store.AddAccount(account)
	h := &Handler{db: db, store: store}
	update := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(id, 10)}}
		c.Request = httptest.NewRequest(http.MethodPatch, "/api/admin/accounts/1/scheduler", strings.NewReader(body))
		h.UpdateAccountScheduler(c)
		return rec
	}
	before, _ := db.SchedulerOutboxHighWatermark(context.Background())
	for _, enabled := range []bool{true, false} {
		if rec := update(`{"codex_bps_enabled":` + strconv.FormatBool(enabled) + `,"codex_bps_profile":"sheets","codex_bps_convergence":"round"}`); rec.Code != 200 {
			t.Fatalf("status=%d: %s", rec.Code, rec.Body.String())
		}
		if got, ok := account.TransportPluginOverride(proxy.BPSPluginID); !ok || got != enabled {
			t.Fatal("saved override not published to the active account")
		}
		if account.EffectiveCodexBPSProfile() != auth.BPSSheets || account.CodexBPSConvergence() != auth.CodexBPSConvergenceRound {
			t.Fatal("profile/convergence not published")
		}
		row, err := db.GetAccountByID(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if value := row.GetCredentialOptionalBool(auth.CodexBPSEnabledCredentialKey); value == nil || *value != enabled {
			t.Fatal("override not persisted")
		}
	}
	events, _ := db.ListSchedulerOutboxEventsAfter(context.Background(), before, 100)
	found := false
	for _, event := range events {
		found = found || (event.EntityType == database.SchedulerEntityAccount && event.EntityID == id)
	}
	if !found {
		t.Fatal("BPS field change did not reach the scheduler outbox")
	}
	if rec := update(`{"codex_bps_enabled":null}`); rec.Code != 200 {
		t.Fatalf("clear status=%d", rec.Code)
	}
	if _, ok := account.TransportPluginOverride(proxy.BPSPluginID); ok {
		t.Fatal("null did not clear the override")
	}
}

func TestCodexBPSSchedulerRejectsIneligibleAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	id := insertTestAccount(t, db)
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: id, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://relay.example", APIKey: "sk"})
	h := &Handler{db: db, store: store}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(id, 10)}}
	c.Request = httptest.NewRequest(http.MethodPatch, "/api/admin/accounts/1/scheduler", strings.NewReader(`{"codex_bps_enabled":true}`))
	h.UpdateAccountScheduler(c)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("relay account accepted BPS: %d %s", rec.Code, rec.Body.String())
	}
}
