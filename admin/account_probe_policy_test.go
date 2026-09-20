package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

func TestAccountProbePolicySchedulerContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	id := insertTestAccount(t, db)
	s := auth.NewStore(db, nil, nil)
	defer s.Stop()
	a := &auth.Account{DBID: id, AccessToken: "token", Status: auth.StatusReady}
	s.AddAccount(a)
	h := &Handler{db: db, store: s}
	patch := func(body string, batch bool) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(id)}}
		c.Request = httptest.NewRequest(http.MethodPatch, "/api/admin/accounts/scheduler", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		if batch {
			h.BatchUpdateAccounts(c)
		} else {
			h.UpdateAccountScheduler(c)
		}
		return w
	}
	for _, invalid := range []string{`{"probe_mode":"bogus"}`, `{"probe_mode":true}`, `{"probe_interval_minutes":-1}`, `{"probe_interval_minutes":1441}`, `{"probe_interval_minutes":1.5}`, `{"probe_interval_minutes":"3"}`} {
		if w := patch(invalid, false); w.Code != 400 {
			t.Fatalf("invalid %s: %d %s", invalid, w.Code, w.Body.String())
		}
	}
	for _, step := range []struct {
		body, mode string
		interval   int
		batch      bool
	}{
		{`{"probe_mode":"off","probe_interval_minutes":1440}`, "off", 1440, false},
		{`{"score_bias_override":1}`, "off", 1440, false},
		{fmt.Sprintf(`{"ids":[%d],"probe_mode":"on","probe_interval_minutes":1}`, id), "on", 1, true},
		{`{"probe_mode":"auto","probe_interval_minutes":0}`, "auto", 0, false},
	} {
		w := patch(step.body, step.batch)
		if w.Code != 200 {
			t.Fatalf("PATCH %s: %d %s", step.body, w.Code, w.Body.String())
		}
		row, err := db.GetAccountByID(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		minutes, _ := row.GetCredentialInt64("probe_interval_minutes")
		if row.GetCredential("probe_mode") != step.mode || minutes != int64(step.interval) {
			t.Fatalf("credentials policy not persisted: mode=%s interval=%d", row.GetCredential("probe_mode"), minutes)
		}
		mode, interval := a.GetProbePolicy()
		if mode != step.mode || interval != step.interval {
			t.Fatalf("runtime stale: %s/%d", mode, interval)
		}
		for _, detail := range []bool{false, true} {
			data, err := json.Marshal(h.buildAccountResponse(row, nil, nil, nil, nil, detail))
			if err != nil {
				t.Fatal(err)
			}
			var body map[string]any
			if err = json.Unmarshal(data, &body); err != nil {
				t.Fatal(err)
			}
			if body["probe_mode"] != step.mode || body["probe_interval_minutes"] != float64(step.interval) {
				t.Fatalf("detail=%v missing policy: %s", detail, data)
			}
		}
	}
}

func TestProbePolicyOffBlocksAdminCallbacks(t *testing.T) {
	s := auth.NewStore(nil, nil, nil)
	defer s.Stop()
	a := &auth.Account{DBID: 1, AccessToken: "token", Status: auth.StatusReady, ProbeMode: "off"}
	s.AddAccount(a)
	calls := 0
	refreshes := 0
	h := &Handler{store: s, probeUsage: func(context.Context, *auth.Account) error { calls++; return nil }, refreshAccount: func(context.Context, int64) error { refreshes++; return nil }}
	h.probeImportedAccountUsage(context.Background(), 1, "test")
	if err := h.refreshAccountByIDWithProbe(context.Background(), 1, true); err != nil {
		t.Fatal(err)
	}
	if calls != 0 || refreshes != 1 {
		t.Fatalf("off probes=%d renewals=%d want 0/1", calls, refreshes)
	}
	if whamDailyUsageBackfillEligible(a) || whamDailyUsageAutoRefreshEligible(a, time.Now()) {
		t.Fatal("off allowed daily WHAM")
	}
	if targets := whamDailyUsageDueTargets([]*auth.Account{a}, map[int64]time.Time{}, time.Now()); len(targets) != 0 {
		t.Fatal("off allowed daily due target")
	}
}
