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

// Imports never enable BPS: new accounts inherit the plugin's default (OFF).
func TestNewCodexAccountCredentialsLeaveBPSUnset(t *testing.T) {
	creds := (&Handler{}).newCodexAccountCredentials(tokenCredentialSeed{refreshToken: "rt", accessToken: "at"})
	for _, key := range codexBPSCredentialKeys {
		if _, ok := creds[key]; ok {
			t.Fatalf("import wrote BPS key %s", key)
		}
	}
}

// A BPS toggle saved through one replica reaches another replica's live
// account through the scheduler outbox, without a restart.
func TestCodexBPSToggleReachesOtherReplica(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	id := insertTestAccount(t, db)
	settings := &database.SystemSettings{MaxConcurrency: 1, SchedulerEngine: "indexed"}
	local, remote := auth.NewStore(db, nil, settings), auth.NewStore(db, nil, settings)
	t.Cleanup(func() { local.Stop(); remote.Stop() })
	for _, store := range []*auth.Store{local, remote} {
		if err := store.Init(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if local.FindByID(id) == nil || remote.FindByID(id) == nil {
		t.Fatal("account not loaded on both replicas")
	}
	h := &Handler{db: db, store: local}
	for _, body := range []string{`{"codex_bps_enabled":true,"codex_bps_profile":"powerpoint"}`, `{"codex_bps_enabled":false}`, `{"codex_bps_enabled":null}`} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(id, 10)}}
		c.Request = httptest.NewRequest(http.MethodPatch, "/api/admin/accounts/1/scheduler", strings.NewReader(body))
		h.UpdateAccountScheduler(c)
		if rec.Code != 200 {
			t.Fatalf("%s: status=%d %s", body, rec.Code, rec.Body.String())
		}
		var want map[string]any
		_ = json.Unmarshal([]byte(body), &want)
		deadline := time.Now().Add(5 * time.Second)
		for {
			enabled, ok := remote.FindByID(id).TransportPluginOverride(proxy.BPSPluginID)
			converged := false
			switch want["codex_bps_enabled"] {
			case nil:
				converged = !ok
			case true:
				converged = ok && enabled && remote.FindByID(id).EffectiveCodexBPSProfile() == auth.BPSPowerPoint
			case false:
				converged = ok && !enabled
			}
			if converged {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s did not reach the other replica", body)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}
