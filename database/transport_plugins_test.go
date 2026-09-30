package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openTransportPluginTestDB(t *testing.T, driver string) *DB {
	t.Helper()
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if driver == "postgres" && dsn == "" {
		t.Skip("PostgreSQL DSN not set")
	}
	if driver == "sqlite" {
		dsn = filepath.Join(t.TempDir(), "plugins.db")
	}
	db, err := New(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// All four usage-log readers and both batch writers are positional; pin the
// transport/plugin_meta columns through every one of them.
func TestUsageLogTransportRoundTrip(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			db := openTransportPluginTestDB(t, driver)
			ctx := context.Background()
			marker := fmt.Sprintf("transport-%d", time.Now().UnixNano())
			db.SetUsageLogConfig(UsageLogModeFull, 100, 300)
			inputs := []UsageLogInput{
				{RequestID: marker + "-0", Endpoint: marker, InboundEndpoint: marker, Model: "m", StatusCode: 200, DaybreakProgram: "d"},
				{RequestID: marker + "-1", Endpoint: marker, InboundEndpoint: marker, Model: "m", StatusCode: 200, DaybreakProgram: "d", Transport: "testplug", PluginMeta: `{"k":"v"}`},
			}
			for i := range inputs {
				if err := db.InsertUsageLog(ctx, &inputs[i]); err != nil {
					t.Fatal(err)
				}
			}
			db.FlushUsageLogs()
			start, end := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
			check := func(logs []*UsageLog, err error, want int) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
				seen := 0
				for _, row := range logs {
					if row.Endpoint != marker {
						continue
					}
					seen++
					if row.DaybreakProgram != "d" {
						t.Fatalf("columns misaligned before transport: %+v", row)
					}
					switch row.RequestID {
					case marker + "-0":
						if row.Transport != TransportNative || row.PluginMeta != "" {
							t.Fatalf("native row = %q/%q", row.Transport, row.PluginMeta)
						}
					case marker + "-1":
						if row.Transport != "testplug" || row.PluginMeta != `{"k":"v"}` {
							t.Fatalf("plugin row = %q/%q", row.Transport, row.PluginMeta)
						}
					}
				}
				if seen != want {
					t.Fatalf("got %d rows, want %d", seen, want)
				}
			}
			logs, err := db.ListRecentUsageLogs(ctx, 5000)
			check(logs, err, 2)
			logs, err = db.ListUsageLogsByTimeRange(ctx, start, end)
			check(logs, err, 2)
			filter := UsageLogFilter{Start: start, End: end, Endpoint: marker, Page: 1, PageSize: 50}
			page, err := db.ListUsageLogsByTimeRangePaged(ctx, filter)
			if err != nil {
				t.Fatal(err)
			}
			check(page.Logs, nil, 2)
			filter.Transport = "testplug"
			logs, err = db.ListUsageLogsByFilter(ctx, filter)
			check(logs, err, 1)
			filter.Transport = TransportNative
			page, err = db.ListUsageLogsByTimeRangePaged(ctx, filter)
			if err != nil {
				t.Fatal(err)
			}
			check(page.Logs, nil, 1)
		})
	}
}

func TestTransportPluginStateSaveEmitsOutboxEvent(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			db := openTransportPluginTestDB(t, driver)
			ctx := context.Background()
			before, err := db.SchedulerOutboxHighWatermark(ctx)
			if err != nil {
				t.Fatal(err)
			}
			id := fmt.Sprintf("p%d", time.Now().UnixNano()%1_000_000_000)
			err = db.SaveTransportPluginState(ctx, TransportPluginState{ID: id, Enabled: true, GroupIDs: []int64{3, 3, -1, 2}, Config: []byte(`{"a":1}`), CaptureEnabled: true, CaptureSampleRate: 0.25})
			if err != nil {
				t.Fatal(err)
			}
			states, err := db.ListTransportPluginStates(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var got *TransportPluginState
			for i := range states {
				if states[i].ID == id {
					got = &states[i]
				}
			}
			if got == nil || !got.Enabled || len(got.GroupIDs) != 2 || string(got.Config) != `{"a":1}` || !got.CaptureEnabled || got.CaptureSampleRate != 0.25 || got.UpdatedAt.IsZero() {
				t.Fatalf("state round trip = %+v", got)
			}
			events, err := db.ListSchedulerOutboxEventsAfter(ctx, before, 100)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, event := range events {
				if event.EntityType == SchedulerEntityPlugin {
					found = true
				}
			}
			if !found {
				t.Fatalf("no plugin outbox event in %+v", events)
			}
			if err := db.SaveTransportPluginState(ctx, TransportPluginState{ID: id, Config: []byte(`[1]`)}); err == nil {
				t.Fatal("non-object config accepted")
			}
			if err := db.SaveTransportPluginState(ctx, TransportPluginState{ID: id, CaptureSampleRate: 1.5}); err == nil {
				t.Fatal("sample rate > 1 accepted")
			}
			if err := db.SaveTransportPluginState(ctx, TransportPluginState{ID: TransportNative}); err == nil {
				t.Fatal("reserved id accepted")
			}
		})
	}
}

func TestAccountTransportPluginOverrideCredential(t *testing.T) {
	db := openTransportPluginTestDB(t, "sqlite")
	ctx := context.Background()
	id, err := db.InsertAccount(ctx, "override", "refresh", "")
	if err != nil {
		t.Fatal(err)
	}
	const key = "transport_plugin_testplug_enabled"
	read := func() *bool {
		t.Helper()
		row, err := db.GetAccountByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return row.GetCredentialOptionalBool(key)
	}
	before, _ := db.SchedulerOutboxHighWatermark(ctx)
	off := false
	if err := db.SetAccountTransportPluginOverride(ctx, id, key, &off); err != nil {
		t.Fatal(err)
	}
	if got := read(); got == nil || *got {
		t.Fatalf("override = %v, want false", got)
	}
	events, _ := db.ListSchedulerOutboxEventsAfter(ctx, before, 100)
	found := false
	for _, event := range events {
		found = found || (event.EntityType == SchedulerEntityAccount && event.EntityID == id)
	}
	if !found {
		t.Fatal("override write did not emit an account outbox event")
	}
	if err := db.SetAccountTransportPluginOverride(ctx, id, key, nil); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != nil {
		t.Fatalf("cleared override = %v, want nil", *got)
	}
	if err := db.SetAccountTransportPluginOverride(ctx, id, "bad-key", nil); err == nil {
		t.Fatal("invalid credential key accepted")
	}
}

func TestPluginCapturesListGetAndRetention(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			db := openTransportPluginTestDB(t, driver)
			ctx := context.Background()
			plugin := fmt.Sprintf("cap%d", time.Now().UnixNano()%1_000_000_000)
			now := time.Now()
			old := now.Add(-PluginCaptureMaxRetention - time.Hour)
			rows := []PluginCapture{
				{Plugin: plugin, RequestID: "r-old", AccountID: 7, Attempt: 1, Direction: PluginCaptureDirectionRequest, Headers: "{}", Body: "old", CreatedAt: old},
				{Plugin: plugin, RequestID: "r-new", AccountID: 7, Attempt: 1, Direction: PluginCaptureDirectionRequest, Headers: "{}", Body: "hello", CreatedAt: now},
				{Plugin: plugin, RequestID: "r-new", AccountID: 7, Attempt: 1, Direction: PluginCaptureDirectionResponse, Status: 502, Body: "bad", ErrorKind: "http_502", Truncated: true, CreatedAt: now},
			}
			if err := db.InsertPluginCaptures(ctx, rows); err != nil {
				t.Fatal(err)
			}
			page, err := db.ListPluginCaptures(ctx, PluginCaptureFilter{Plugin: plugin})
			if err != nil {
				t.Fatal(err)
			}
			if page.Total != 3 || len(page.Captures) != 3 || page.Captures[0].Body != "" {
				t.Fatalf("list = %+v", page)
			}
			status := 502
			page, err = db.ListPluginCaptures(ctx, PluginCaptureFilter{Plugin: plugin, RequestID: "r-new", Status: &status})
			if err != nil || page.Total != 1 || !page.Captures[0].Truncated || page.Captures[0].BodyBytes != 3 {
				t.Fatalf("filtered list = %+v, %v", page, err)
			}
			got, err := db.GetPluginCapture(ctx, page.Captures[0].ID)
			if err != nil || got == nil || got.Body != "bad" || got.ErrorKind != "http_502" {
				t.Fatalf("get = %+v, %v", got, err)
			}
			page, err = db.ListPluginCaptures(ctx, PluginCaptureFilter{Plugin: plugin, Start: now.Add(-time.Hour)})
			if err != nil || page.Total != 2 {
				t.Fatalf("time filter = %+v, %v", page, err)
			}
			cutoff := now.Add(-PluginCaptureMaxRetention)
			result, err := db.PurgePluginCaptures(ctx, cutoff, 1)
			if err != nil {
				t.Fatal(err)
			}
			if result.Deleted < 1 || result.Batches < 2 {
				t.Fatalf("purge = %+v", result)
			}
			page, err = db.ListPluginCaptures(ctx, PluginCaptureFilter{Plugin: plugin})
			if err != nil || page.Total != 2 {
				t.Fatalf("after purge = %+v, %v", page, err)
			}
			if missing, err := db.GetPluginCapture(ctx, -1); err != nil || missing != nil {
				t.Fatalf("missing capture = %+v, %v", missing, err)
			}
		})
	}
}
