package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/codex2api/proxy/plugins"
	"github.com/gin-gonic/gin"
)

func runBatchUpdate(t *testing.T, h *Handler, body string) (int, map[string]interface{}) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/batch-update", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.BatchUpdateAccounts(c)
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// Batch BPS settings reach the live accounts (the plugin override
// included) and skip accounts that cannot use BPS.
func TestBatchUpdateAccountsSetsBPSFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	ctx := context.Background()
	codex1, _ := db.InsertAccount(ctx, "codex-1", "rt_1", "")
	codex2, _ := db.InsertAccount(ctx, "codex-2", "rt_2", "")
	grok, _ := db.InsertAccount(ctx, "grok-1", "rt_3", "")
	store := auth.NewStore(nil, nil, nil)
	store.AddAccount(&auth.Account{DBID: codex1, AccessToken: "t1", Status: auth.StatusReady})
	store.AddAccount(&auth.Account{DBID: codex2, AccessToken: "t2", Status: auth.StatusReady})
	store.AddAccount(&auth.Account{DBID: grok, APIKey: "k", UpstreamType: auth.UpstreamGrok, Status: auth.StatusReady})
	h := &Handler{db: db, store: store}

	code, out := runBatchUpdate(t, h, fmt.Sprintf(`{"ids":[%d,%d,%d],"codex_bps_enabled":true,"codex_bps_profile":"excel","codex_bps_convergence":"turn_round","codex_native_enabled":false,"codex_bps_image_trim_enabled":true,"codex_bps_models":["gpt-6-*"]}`, codex1, codex2, grok))
	if code != http.StatusOK || out["success"] != float64(2) || out["failed"] != float64(1) {
		t.Fatalf("batch BPS: %d %v (the Grok account is skipped)", code, out)
	}
	for _, id := range []int64{codex1, codex2} {
		enabled, ok := store.FindByID(id).TransportPluginOverride(proxy.BPSPluginID)
		if !ok || !enabled {
			t.Fatalf("account %d plugin override = %v/%v, want forced on", id, enabled, ok)
		}
		row, err := db.GetAccountByID(ctx, id)
		if err != nil || row.GetCredential(auth.CodexBPSProfileCredentialKey) != "excel" || row.GetCredential(auth.CodexBPSConvergenceCredentialKey) != "turn_round" {
			t.Fatalf("account %d credentials not saved: %v %+v", id, err, row)
		}
	}
	if _, ok := store.FindByID(grok).TransportPluginOverride(proxy.BPSPluginID); ok {
		t.Fatal("the Grok account got a BPS override")
	}

	code, out = runBatchUpdate(t, h, fmt.Sprintf(`{"ids":[%d],"codex_bps_enabled":null}`, codex1))
	if code != http.StatusOK || out["success"] != float64(1) {
		t.Fatalf("batch inherit: %d %v", code, out)
	}
	if _, ok := store.FindByID(codex1).TransportPluginOverride(proxy.BPSPluginID); ok {
		t.Fatal("null clears the override back to inherit")
	}
}

// Enable-by-group end to end: add accounts to a group in batch (keeping
// their other groups), put the group on the plugin's list, and the plugin
// serves them with the global switch off.
func TestBatchAddToGroupEnablesPluginByGroup(t *testing.T) {
	router, db, store, accountID := newTransportPluginAdminRouter(t)
	ctx := context.Background()
	other, _ := db.InsertAccount(ctx, "outside", "rt_out", "")
	store.AddAccount(&auth.Account{DBID: other, AccessToken: "t", Status: auth.StatusReady})
	existing, err := db.CreateAccountGroup(ctx, "Existing", "", "#2563eb", 0, 0, sql.NullInt64{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	bpsGroup, err := db.CreateAccountGroup(ctx, "BPS", "", "#16a34a", 0, 0, sql.NullInt64{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db, store: store}
	if code, out := runBatchUpdate(t, h, fmt.Sprintf(`{"ids":[%d],"group_ids":[%d]}`, accountID, existing)); code != http.StatusOK {
		t.Fatalf("seed group: %d %v", code, out)
	}
	if code, out := runBatchUpdate(t, h, fmt.Sprintf(`{"ids":[%d],"add_group_ids":[%d]}`, accountID, bpsGroup)); code != http.StatusOK || out["success"] != float64(1) {
		t.Fatalf("add to group: %d %v", code, out)
	}
	groups, err := db.GetAccountGroupIDs(ctx, accountID)
	if err != nil || len(groups) != 2 {
		t.Fatalf("groups = %v %v, want the existing group kept and the BPS group added", groups, err)
	}
	if code, _ := runBatchUpdate(t, h, fmt.Sprintf(`{"ids":[%d],"add_group_ids":[%d],"group_ids":[%d]}`, accountID, bpsGroup, existing)); code != http.StatusBadRequest {
		t.Fatalf("add_group_ids with group_ids: %d", code)
	}

	if rec := doTransportPluginRequest(t, router, http.MethodPut, "/api/admin/plugins/adminplug", fmt.Sprintf(`{"enabled":false,"group_ids":[%d]}`, bpsGroup)); rec.Code != http.StatusOK {
		t.Fatalf("enable by group: %d %s", rec.Code, rec.Body.String())
	}
	p, _ := plugins.Default().Get("adminplug")
	if !plugins.Default().EnabledFor(p, store.FindByID(accountID)) {
		t.Fatal("an account in an enabled group is served with the global switch off")
	}
	if plugins.Default().EnabledFor(p, store.FindByID(other)) {
		t.Fatal("an account outside the group is not")
	}
}
