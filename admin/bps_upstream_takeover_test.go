package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// Upstream's Basispoints account mode writes the BPS plugin override.
func TestExcelBPSModeIsThePluginOverride(t *testing.T) {
	for _, tc := range []struct {
		name            string
		enabled, optOut string
		want            any
	}{
		{"on", `true`, `false`, true},
		{"off", `false`, `true`, false},
		{"inherit", `false`, `false`, nil},
	} {
		update, err := parseAccountSchedulerUpdate(updateAccountSchedulerReq{ExcelBPSEnabled: json.RawMessage(tc.enabled), ExcelBPSOptOut: json.RawMessage(tc.optOut)})
		if err != nil {
			t.Fatal(err)
		}
		got, ok := update.CredentialUpdates[auth.CodexBPSEnabledCredentialKey]
		if !ok || got != tc.want {
			t.Fatalf("%s: codex_bps_enabled = %#v (set %v), want %#v", tc.name, got, ok, tc.want)
		}
		if profile, set := update.CredentialUpdates[auth.CodexBPSProfileCredentialKey]; (tc.name == "on") != set || set && profile != string(auth.BPSExcel) {
			t.Fatalf("%s: profile = %#v", tc.name, profile)
		}
	}

	// The legacy keys are accepted, never stored: the plugin override is the
	// only per-account input.
	for _, req := range []updateAccountSchedulerReq{
		{ExcelBPSEnabled: json.RawMessage(`true`)},
		{ExcelBPSOptOut: json.RawMessage(`true`)},
		{codexBPSAccountFieldsReq: codexBPSAccountFieldsReq{Enabled: json.RawMessage(`false`)}},
	} {
		update, err := parseAccountSchedulerUpdate(req)
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{auth.ExcelBPSCredentialKey, auth.ExcelBPSOptOutCredentialKey} {
			if _, written := update.CredentialUpdates[key]; written {
				t.Fatalf("%s written: %#v", key, update.CredentialUpdates)
			}
		}
	}
}

func TestExcelBPSModeViewFollowsThePluginOverride(t *testing.T) {
	row := func(creds map[string]any) *database.AccountRow {
		return &database.AccountRow{Credentials: creds}
	}
	for _, tc := range []struct {
		creds           map[string]any
		enabled, optOut bool
	}{
		{map[string]any{}, false, false},
		{map[string]any{auth.CodexBPSEnabledCredentialKey: true}, true, false},
		{map[string]any{auth.CodexBPSEnabledCredentialKey: false}, false, true},
		{map[string]any{auth.ExcelBPSCredentialKey: true}, false, false}, // legacy key is not an input
	} {
		enabled, optOut := excelBPSModeFlags(row(tc.creds))
		if enabled != tc.enabled || optOut != tc.optOut {
			t.Fatalf("%v: mode = %v/%v", tc.creds, enabled, optOut)
		}
	}
	if bpsPluginServes(nil) {
		t.Fatal("nil account is never served")
	}
	on := true
	store := auth.NewStore(nil, nil, nil)
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: 5, AccessToken: "at", Status: auth.StatusReady})
	store.ApplyAccountTransportPluginOverride(5, proxy.BPSPluginID, &on)
	if !bpsPluginServes(store.FindByID(5)) {
		t.Fatal("the effective flag is the plugin serving the account")
	}
}

// The BPS switch has one input per account: a leftover openai_excel_bps flag
// no longer forces the plugin on over an explicit "off".
func TestLegacyExcelFlagNoLongerOverridesThePluginSwitch(t *testing.T) {
	off := false
	store := auth.NewStore(nil, nil, nil)
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: 7, AccessToken: "at", Status: auth.StatusReady, ExcelBPSEnabled: true})
	store.ApplyAccountTransportPluginOverride(7, proxy.BPSPluginID, &off)
	if bpsPluginServes(store.FindByID(7)) {
		t.Fatal("openai_excel_bps overrode the plugin's per-account off")
	}
}

// The Plugins-page account switch for BPS goes through the same helper and
// checks as the account dialogs: persisted, published, and refused for
// accounts that cannot use BPS.
func TestBPSPluginAccountSwitchSharesTheAccountWritePath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	attachBPSPluginForTest(t, db)
	oauth := insertTestAccount(t, db)
	relay := insertTestAccount(t, db)
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: oauth, AccessToken: "token", Status: auth.StatusReady})
	store.AddAccount(&auth.Account{DBID: relay, UpstreamType: auth.UpstreamOpenAIResponses, APIKey: "sk-relay", BaseURL: "https://relay.example", Status: auth.StatusReady})
	h := &Handler{db: db, store: store}
	router := gin.New()
	h.registerTransportPluginRoutes(router.Group("/api/admin"))
	path := func(id int64) string { return "/api/admin/plugins/bps/accounts/" + strconv.FormatInt(id, 10) }

	if rec := doTransportPluginRequest(t, router, http.MethodPut, path(oauth), `{"enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("off: %d %s", rec.Code, rec.Body.String())
	}
	if enabled, ok := store.FindByID(oauth).TransportPluginOverride(proxy.BPSPluginID); !ok || enabled {
		t.Fatal("override not published to the live account")
	}
	row, err := db.GetAccountByID(context.Background(), oauth)
	if err != nil {
		t.Fatal(err)
	}
	if v := row.GetCredentialOptionalBool(auth.CodexBPSEnabledCredentialKey); v == nil || *v {
		t.Fatalf("codex_bps_enabled not persisted: %v", v)
	}
	if rec := doTransportPluginRequest(t, router, http.MethodPut, path(relay), `{"enabled":true}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("relay account forced on: %d %s", rec.Code, rec.Body.String())
	}
}
