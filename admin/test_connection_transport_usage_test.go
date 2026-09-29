package admin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// connectionTransportFixture routes every upstream call through a Resin
// stub: BPS and native Codex both answer a completed stream.
type connectionTransportFixture struct {
	h       *Handler
	db      *database.DB
	account *auth.Account
	bps     atomic.Int32
	native  atomic.Int32
}

func newConnectionTransportFixture(t *testing.T) *connectionTransportFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	f := &connectionTransportFixture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "bps.openai.com/basispoints/api/responses"):
			f.bps.Add(1)
		case strings.Contains(r.URL.Path, "/codex/responses"):
			f.native.Add(1)
		default:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"item_id\":\"m\",\"delta\":\"ok\"}\n\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_t\",\"object\":\"response\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":1,\"total_tokens\":4}}}\n\n")
	}))
	t.Cleanup(server.Close)
	oldResin := proxy.GetResinConfig()
	t.Cleanup(func() { proxy.SetResinConfig(oldResin) })
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "transport-usage-test"})

	f.db = newTestAdminDB(t)
	f.db.SetUsageLogConfig(database.UsageLogModeFull, 100, 300)
	id := insertTestAccount(t, f.db)
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.6-sol", MaxRetries: 1})
	t.Cleanup(store.Stop)
	f.account = &auth.Account{DBID: id, AccessToken: "test-token", AccountID: "acct-transport", PlanType: "pro", Status: auth.StatusReady}
	store.AddAccount(f.account)
	f.h = &Handler{store: store, db: f.db}
	return f
}

func (f *connectionTransportFixture) enableBPS(on bool) {
	f.h.store.ApplyAccountTransportPluginOverride(f.account.ID(), proxy.BPSPluginID, &on)
}

// lastRow returns the newest connection_test usage row of the account.
func (f *connectionTransportFixture) lastRow(t *testing.T) *database.UsageLog {
	t.Helper()
	f.db.FlushUsageLogs()
	accountID := f.account.ID()
	logs, err := f.db.ListUsageLogsByFilter(context.Background(), database.UsageLogFilter{Start: time.Now().Add(-time.Minute), End: time.Now().Add(time.Minute), AccountID: &accountID})
	if err != nil {
		t.Fatal(err)
	}
	var got *database.UsageLog
	for _, row := range logs {
		if row.InternalReason == internalReasonConnectionTest && (got == nil || row.ID > got.ID) {
			got = row
		}
	}
	if got == nil {
		t.Fatalf("no connection_test usage row: %+v", logs)
	}
	return got
}

func assertBPSConnectionRow(t *testing.T, row *database.UsageLog) {
	t.Helper()
	if row.Transport != proxy.BPSPluginID || !strings.Contains(row.UpstreamEndpoint, "/basispoints/api/responses") || gjson.Get(row.PluginMeta, "profile").String() == "" {
		t.Fatalf("BPS-served test row = transport %q endpoint %q meta %q", row.Transport, row.UpstreamEndpoint, row.PluginMeta)
	}
}

func TestSingleConnectionTestRecordsTheServingTransport(t *testing.T) {
	f := newConnectionTransportFixture(t)
	f.enableBPS(true)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(f.account.ID(), 10)}}
	c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/1/test", nil)
	f.h.TestConnection(c)
	if f.bps.Load() != 1 || f.native.Load() != 0 {
		t.Fatalf("upstream calls bps=%d native=%d: %s", f.bps.Load(), f.native.Load(), w.Body.String())
	}
	assertBPSConnectionRow(t, f.lastRow(t))
}

func TestBatchConnectionTestRecordsTheServingTransport(t *testing.T) {
	f := newConnectionTransportFixture(t)
	f.enableBPS(false)
	if status, msg := f.h.runSingleBatchTest(context.Background(), f.account); status != "success" {
		t.Fatalf("native batch test = %s %s", status, msg)
	}
	if row := f.lastRow(t); row.Transport != database.TransportNative || row.UpstreamEndpoint != "/v1/responses" || row.StatusCode != http.StatusOK {
		t.Fatalf("native batch row = transport %q endpoint %q status %d", row.Transport, row.UpstreamEndpoint, row.StatusCode)
	}
	f.enableBPS(true)
	if status, msg := f.h.runSingleBatchTest(context.Background(), f.account); status != "success" {
		t.Fatalf("BPS batch test = %s %s", status, msg)
	}
	if f.bps.Load() != 1 {
		t.Fatalf("BPS calls = %d, want 1", f.bps.Load())
	}
	assertBPSConnectionRow(t, f.lastRow(t))
}

func TestRecycleBinConnectionTestRecordsTheServingTransport(t *testing.T) {
	f := newConnectionTransportFixture(t)
	f.enableBPS(true)
	if status, msg := f.h.runRecycleBinSingleTest(context.Background(), f.account); status != "success" {
		t.Fatalf("recycle-bin test = %s %s", status, msg)
	}
	if f.bps.Load() != 1 || f.native.Load() != 0 {
		t.Fatalf("upstream calls bps=%d native=%d", f.bps.Load(), f.native.Load())
	}
	assertBPSConnectionRow(t, f.lastRow(t))
}
