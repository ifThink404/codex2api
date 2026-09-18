package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// 账号级策略三列默认 inherit，本任务不改变任何行为。这里钉住 admin 这一层的四件事：
// PATCH 能落库、非法枚举 400（而不是静默归 inherit）、未带的字段不被覆写、
// 以及写库之后内存账号被热更新（否则要等下一次 outbox 才生效）。
func TestUpdateAccountSchedulerPersistsAccountPolicies(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	runtimeAccount := &auth.Account{
		DBID:        accountID,
		AccessToken: "token",
		Status:      auth.StatusReady,
		PlanType:    "pro",
	}
	store := &auth.Store{}
	store.AddAccount(runtimeAccount)
	handler := &Handler{db: db, store: store}

	patch := func(t *testing.T, body string) *httptest.ResponseRecorder {
		t.Helper()
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", accountID)}}
		ctx.Request = httptest.NewRequest(http.MethodPatch,
			fmt.Sprintf("/api/admin/accounts/%d/scheduler", accountID), strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		handler.UpdateAccountScheduler(ctx)
		return recorder
	}

	// 默认值：还没 PATCH 过的账号必须是 inherit，且 GET 负载里带上三列。
	row, err := db.GetAccountByID(context.Background(), accountID)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	payload := marshalAccountPolicyFields(t, handler, row)
	for _, field := range []string{"prompt_filter_policy", "egress_policy", "session_guards_policy"} {
		if payload[field] != "inherit" {
			t.Fatalf("GET payload %s = %q, want inherit", field, payload[field])
		}
	}

	if recorder := patch(t, `{"egress_policy":"direct"}`); recorder.Code != http.StatusOK {
		t.Fatalf("PATCH egress_policy=direct status = %d, want %d (body %s)",
			recorder.Code, http.StatusOK, recorder.Body.String())
	}

	row, err = db.GetAccountByID(context.Background(), accountID)
	if err != nil {
		t.Fatalf("GetAccountByID after patch: %v", err)
	}
	if row.EgressPolicy != "direct" {
		t.Fatalf("egress_policy = %q, want direct", row.EgressPolicy)
	}
	// 没带的两列不能被这次 PATCH 顺手写成别的值。
	if row.PromptFilterPolicy != "inherit" || row.SessionGuardsPolicy != "inherit" {
		t.Fatalf("untouched policies changed: %+v", row)
	}
	payload = marshalAccountPolicyFields(t, handler, row)
	if payload["egress_policy"] != "direct" {
		t.Fatalf("GET payload egress_policy = %q, want direct", payload["egress_policy"])
	}

	// 热更新：写库之后内存账号必须立刻反映出来。
	if acc := handler.store.FindByID(accountID); acc == nil || !acc.EgressDirect() {
		t.Fatal("runtime account did not pick up egress_policy=direct")
	}
	if runtimeAccount.PromptFilterExempt() || runtimeAccount.SessionGuardsOff() {
		t.Fatal("runtime account picked up policies that were never patched")
	}

	// 非法枚举必须 400，并且不能改动已存的值。
	if recorder := patch(t, `{"egress_policy":"pool"}`); recorder.Code != http.StatusBadRequest {
		t.Fatalf("PATCH egress_policy=pool status = %d, want %d (body %s)",
			recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	row, err = db.GetAccountByID(context.Background(), accountID)
	if err != nil {
		t.Fatalf("GetAccountByID after rejected patch: %v", err)
	}
	if row.EgressPolicy != "direct" {
		t.Fatalf("rejected patch changed egress_policy to %q", row.EgressPolicy)
	}

	// 不带策略字段的 PATCH 不能把三列重置回 inherit。
	if recorder := patch(t, `{"score_bias_override":12}`); recorder.Code != http.StatusOK {
		t.Fatalf("PATCH score_bias_override status = %d, want %d (body %s)",
			recorder.Code, http.StatusOK, recorder.Body.String())
	}
	row, err = db.GetAccountByID(context.Background(), accountID)
	if err != nil {
		t.Fatalf("GetAccountByID after unrelated patch: %v", err)
	}
	if row.EgressPolicy != "direct" {
		t.Fatalf("unrelated patch reset egress_policy to %q", row.EgressPolicy)
	}

	// 其余两列同样可写，且大小写/空白要被收敛。
	if recorder := patch(t, `{"prompt_filter_policy":" EXEMPT ","session_guards_policy":"Off"}`); recorder.Code != http.StatusOK {
		t.Fatalf("PATCH remaining policies status = %d, want %d (body %s)",
			recorder.Code, http.StatusOK, recorder.Body.String())
	}
	row, err = db.GetAccountByID(context.Background(), accountID)
	if err != nil {
		t.Fatalf("GetAccountByID after remaining patch: %v", err)
	}
	if row.PromptFilterPolicy != "exempt" || row.SessionGuardsPolicy != "off" {
		t.Fatalf("remaining policies = %+v, want exempt/off", row)
	}
	if !runtimeAccount.PromptFilterExempt() || !runtimeAccount.SessionGuardsOff() {
		t.Fatal("runtime account did not pick up the remaining policies")
	}

	// null 语义与其他字段一致：显式清空，收敛成 inherit 而不是空串。
	if recorder := patch(t, `{"prompt_filter_policy":null}`); recorder.Code != http.StatusOK {
		t.Fatalf("PATCH prompt_filter_policy=null status = %d, want %d (body %s)",
			recorder.Code, http.StatusOK, recorder.Body.String())
	}
	row, err = db.GetAccountByID(context.Background(), accountID)
	if err != nil {
		t.Fatalf("GetAccountByID after null patch: %v", err)
	}
	if row.PromptFilterPolicy != "inherit" {
		t.Fatalf("prompt_filter_policy after null = %q, want inherit", row.PromptFilterPolicy)
	}
}

func marshalAccountPolicyFields(t *testing.T, handler *Handler, row *database.AccountRow) map[string]string {
	t.Helper()
	encoded, err := json.Marshal(handler.buildAccountResponse(row, nil, nil, nil, nil, true))
	if err != nil {
		t.Fatalf("marshal account response: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal account response: %v", err)
	}
	out := make(map[string]string, 3)
	for _, field := range []string{"prompt_filter_policy", "egress_policy", "session_guards_policy"} {
		raw, ok := decoded[field]
		if !ok {
			t.Fatalf("account response is missing %s", field)
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatalf("decode %s: %v", field, err)
		}
		out[field] = value
	}
	return out
}
