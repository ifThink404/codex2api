package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy/plugins"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// The first start on production's data (upstream's Excel Basispoints live
// through codex_basispoints_enabled, 3 accounts with the old local
// codex_bps_enabled=false, 8 without it, no native route set, and no plugin
// row) keeps today's behaviour: every eligible account is served by BPS with
// the Excel profile, BPS goes first, and a BPS failure before output falls
// back to the same account's native route.
func TestProductionExcelShapeRoutesBPSFirstWithSameAccountNativeFallback(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"5xx", http.StatusBadGateway, `{"error":{"message":"upstream failed","type":"server_error"}}`},
		{"usage-policy 403", http.StatusForbidden, bpsPolicyBlockBody},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			t.Setenv("CODEX_TRANSPORT_MODE", "standard")
			freshBPSAccountStates(t)
			ctx := context.Background()
			db, err := database.New("sqlite", filepath.Join(t.TempDir(), "production-shape.db"))
			require.NoError(t, err)
			db.SetUsageLogConfig(database.UsageLogModeFull, 100, 300)
			var ids []int64
			for i := 0; i < 11; i++ {
				creds := map[string]any{"access_token": fmt.Sprintf("at-%d", i), "refresh_token": fmt.Sprintf("rt-%d", i), "account_id": fmt.Sprintf("acct-%d", i), "plan_type": "pro"}
				if i < 3 {
					creds[auth.CodexBPSEnabledCredentialKey] = false
				}
				id, err := db.InsertAccountWithCredentials(ctx, fmt.Sprintf("prod-%d", i), creds, "")
				require.NoError(t, err)
				ids = append(ids, id)
			}
			settings, err := db.GetSystemSettings(ctx)
			require.NoError(t, err)
			if settings == nil {
				settings = &database.SystemSettings{}
			}
			settings.CodexBasispointsEnabled = true
			require.NoError(t, db.UpdateSystemSettings(ctx, settings))

			// Startup: the plugin's Migrate runs the legacy, unify and parity steps.
			registry := plugins.NewRegistry()
			registry.Register(bpsPlugin{})
			require.NoError(t, registry.Attach(ctx, db))
			previous := plugins.SwapDefault(registry)
			previousConfig := currentBPSConfig()
			store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, MaxRetries: 1})
			t.Cleanup(func() {
				store.Stop()
				plugins.SwapDefault(previous)
				auth.SetTransportPluginReloader(nil)
				storeBPSConfig(previousConfig)
				_ = db.Close()
			})
			require.NoError(t, store.Init(ctx))
			require.True(t, currentBPSConfig().PrefersBPS(), "BPS first, native is the fallback")
			p, _ := registry.Get(BPSPluginID)
			for _, id := range ids {
				account := store.FindByID(id)
				require.NotNil(t, account)
				require.True(t, registry.EnabledFor(p, account), "account %d is served by BPS through the global switch", id)
				require.True(t, account.CodexNativeRouteExplicit(), "account %d keeps a native route", id)
				require.Equal(t, auth.BPSExcel, account.EffectiveCodexBPSProfile(), "account %d uses the Excel profile", id)
			}

			var bpsCalls, nativeCalls []int64
			for _, id := range ids {
				account := store.FindByID(id)
				account.Status = auth.StatusReady
				accountID := id
				installClaudeBoundaryTransport(t, account, func(r *http.Request) (*http.Response, error) {
					if strings.HasPrefix(r.URL.String(), CodexBPSBaseURL) {
						bpsCalls = append(bpsCalls, accountID)
						return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(tc.body)), Request: r}, nil
					}
					nativeCalls = append(nativeCalls, accountID)
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(nativeSSE("gpt-6-sol"))), Request: r}, nil
				})
			}
			handler := NewHandler(store, db, &config.Config{AllowAnonymousV1: true}, nil)
			handler.SetRuntimeCache(cache.NewMemory(1))
			f := &bpsHandlerFixture{db: db, store: store, handler: handler, registry: registry}
			recorder := f.serve(t, "/v1/responses", `{"model":"gpt-6-sol","stream":true,"input":"hi"}`)
			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			require.Len(t, bpsCalls, 1, "BPS first")
			require.Equal(t, bpsCalls, nativeCalls, "then the same account's native route")
			var sent string
			for _, row := range f.usageRows(t) {
				if row.Transport == BPSPluginID {
					sent = gjson.Get(row.PluginMeta, "profile").String()
				}
			}
			require.Equal(t, "excel", sent)
		})
	}
}
