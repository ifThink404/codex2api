package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func telemetryRuntimeForTest(test *testing.T, enabled bool) {
	test.Helper()
	previous := CurrentRuntimeSettings()
	test.Cleanup(func() { ApplyRuntimeSettings(previous) })
	next := previous
	next.CodexTelemetryEnabled = enabled
	ApplyRuntimeSettings(next)
	test.Setenv("CODEX_TELEMETRY_ENABLED", "true")
}

func TestCodexAnalyticsMetadataPolicyAndIdentity(test *testing.T) {
	for _, scenario := range []struct {
		name    string
		enabled bool
		envOff  bool
		optOut  bool
		want    bool
	}{
		{"default_off", false, false, false, false},
		{"enabled", true, false, false, true},
		{"deployment_off", true, true, false, false},
		{"client_opt_out", true, false, true, false},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			telemetryRuntimeForTest(test, scenario.enabled)
			if scenario.envOff {
				test.Setenv("CODEX_TELEMETRY_ENABLED", "false")
			}
			for _, carrier := range []string{"string", "object", "header", "flat", "missing"} {
				test.Run(carrier, func(test *testing.T) {
					headers := http.Header{}
					body := []byte(`{"model":"test","input":[{"type":"compaction","id":"opaque","encrypted_content":"unchanged"}]}`)
					metadata := `{"session_id":"session","thread_id":"child","window_id":"child:71","window_number":71,"parent_thread_id":"parent","forked_from_thread_id":"fork","request_kind":"turn","unknown":{"kept":true},"analytics_enabled":true}`
					if scenario.optOut {
						metadata, _ = sjson.Set(metadata, "analytics_enabled", false)
					}
					switch carrier {
					case "string":
						body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata", metadata)
					case "object":
						body, _ = sjson.SetRawBytes(body, "client_metadata.x-codex-turn-metadata", []byte(metadata))
					case "header":
						headers.Set(codexTurnMetadataHeader, metadata)
						headers.Set(codexSessionIDHeader, "session")
						headers.Set(codexThreadIDHeader, "child")
					case "flat":
						body, _ = sjson.SetRawBytes(body, "client_metadata", []byte(`{"session_id":"session","thread_id":"child","window_id":"child:71","parent_thread_id":"parent"}`))
						headers.Set("X-Codex-Turn-State", "current-turn-state")
						headers.Set("X-Client-Request-Id", "request-1")
					}
					originalBody, originalHeaders := bytes.Clone(body), headers.Clone()
					prepared, outbound := ApplyCodexAnalyticsMetadata(body, headers)
					actual := codexTurnMetadata(prepared, nil)
					want := scenario.want
					if scenario.optOut && (carrier == "flat" || carrier == "missing") {
						want = scenario.enabled && !scenario.envOff
					}
					require.Equal(test, want, actual.Get("analytics_enabled").Bool())
					require.Contains(test, []gjson.Type{gjson.False, gjson.True}, actual.Get("analytics_enabled").Type)
					require.Equal(test, actual.Raw, outbound.Get(codexTurnMetadataHeader))
					require.Equal(test, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(prepared, "input").Raw)
					reprojected := CodexRequestMetadataHeaders(outbound, prepared)
					if carrier != "missing" {
						require.Equal(test, "session", actual.Get("session_id").String())
						require.Equal(test, "child", actual.Get("thread_id").String())
						require.Equal(test, "session", reprojected.Get(codexSessionIDHeader))
					}
					if carrier == "flat" {
						require.Equal(test, "current-turn-state", reprojected.Get("X-Codex-Turn-State"))
						require.Equal(test, "request-1", reprojected.Get("X-Client-Request-Id"))
					} else if carrier != "missing" {
						require.True(test, actual.Get("unknown.kept").Bool())
						require.Equal(test, "fork", actual.Get("forked_from_thread_id").String())
					}
					require.Equal(test, carrier == "object", gjson.GetBytes(prepared, "client_metadata.x-codex-turn-metadata").IsObject())
					repeated, repeatedHeaders := ApplyCodexAnalyticsMetadata(prepared, outbound)
					require.JSONEq(test, string(prepared), string(repeated))
					require.Equal(test, reprojected, repeatedHeaders)
					require.Equal(test, originalBody, body)
					require.Equal(test, originalHeaders, headers)
				})
			}
		})
	}
}

func TestCodexAnalyticsHTTPAndCompactFinalTransmission(test *testing.T) {
	telemetryRuntimeForTest(test, false)
	previousResin := GetResinConfig()
	test.Cleanup(func() { SetResinConfig(previousResin) })
	type capture struct {
		headers http.Header
		body    []byte
	}
	received := make(chan capture, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		received <- capture{request.Header.Clone(), readUpstreamRequestBody(request)}
		_, _ = writer.Write([]byte(`{"status":"completed","output":[]}`))
	}))
	test.Cleanup(server.Close)
	SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "analytics-metadata"})
	for _, endpoint := range []string{"responses", "compact"} {
		for _, compression := range []string{"off", "zstd"} {
			test.Run(endpoint+"/"+compression, func(test *testing.T) {
				test.Setenv("CODEX_REQUEST_COMPRESSION", compression)
				test.Setenv("CODEX_OUTBOUND_SESSION_MODE", "preserve")
				account := &auth.Account{DBID: 4001, AccessToken: "test-token", AccountID: "test-account", CodexFingerprintMode: auth.CodexFingerprintModeDevice,
					CustomHeaders: map[string]string{"X-Codex-Turn-Metadata": `{"analytics_enabled":true,"session_id":"wrong"}`}}
				headers, body := fingerprintMetadataFixture(test, "session", "thread", "parent", "fork", 71)
				body, _ = sjson.SetBytes(body, "input", []any{})
				var response *http.Response
				var err error
				if endpoint == "compact" {
					response, err = ExecuteCompactRequest(context.Background(), account, body, "cache", "", "key", nil, headers)
				} else {
					response, err = ExecuteRequest(context.Background(), account, body, "cache", "", "key", nil, headers, false)
				}
				require.NoError(test, err)
				_, err = io.Copy(io.Discard, response.Body)
				require.NoError(test, err)
				require.NoError(test, response.Body.Close())
				sent := <-received
				metadata := codexTurnMetadata(sent.body, sent.headers)
				if endpoint == "compact" {
					require.False(test, gjson.GetBytes(sent.body, "client_metadata").Exists())
				}
				require.Equal(test, gjson.False, metadata.Get("analytics_enabled").Type)
				require.JSONEq(test, metadata.Raw, sent.headers.Get(codexTurnMetadataHeader))
				require.Equal(test, "session", metadata.Get("session_id").String())
				require.Equal(test, "session", sent.headers.Get(codexSessionIDHeader))
				diagnostic, err := json.Marshal(outboundMetadataJSON(metadata))
				require.NoError(test, err)
				require.Equal(test, gjson.False, gjson.GetBytes(diagnostic, "analytics_enabled").Type)
			})
		}
	}
}

func TestCodexTelemetryUsesFinalIdentityAndHonorsOptOut(test *testing.T) {
	telemetryRuntimeForTest(test, true)
	previousManager := codexTelemetryGlobal
	test.Cleanup(func() { codexTelemetryGlobal = previousManager })
	manager := newCodexTelemetryManager()
	manager.once.Do(func() {})
	codexTelemetryGlobal = manager
	account := &auth.Account{DBID: 77, AccessToken: "original-token", AccountID: "original-account", CodexFingerprintMode: auth.CodexFingerprintModeFull}
	headers := http.Header{"User-Agent": {"codex-tui/0.154.0 (Windows 10.0.26200; x86_64) WindowsTerminal (codex-tui; 0.154.0)"}, "Originator": {"codex-tui"}, "Version": {"0.154.0"}, "Authorization": {"Bearer final-token"}, "Chatgpt-Account-Id": {"final-account"}}
	body := []byte(`{"model":"test","client_metadata":{"x-codex-turn-metadata":{"session_id":"final-session","thread_id":"final-thread","turn_id":"turn","analytics_enabled":true}}}`)
	attempt := beginCodexTelemetry(codexTelemetryRequest{account: account, body: body, headers: headers, proxyOverride: "http://egress.invalid:8080"})
	require.NotNil(test, attempt)
	require.Equal(test, "final-session", attempt.profile.sessionID)
	require.Equal(test, "final-thread", attempt.profile.threadID)
	require.Equal(test, "final-token", attempt.profile.client.accessToken)
	require.Equal(test, "final-account", attempt.profile.client.accountID)
	require.Equal(test, headers.Get("User-Agent"), attempt.profile.client.userAgent)
	require.Equal(test, "http://egress.invalid:8080", attempt.profile.client.proxyURL)
	require.Len(test, manager.queue, 2)
	for range 2 {
		<-manager.queue
	}
	response := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"))}
	attempt.observeResult(response, nil)
	_, err := io.ReadAll(response.Body)
	require.NoError(test, err)
	require.NoError(test, response.Body.Close())
	require.Len(test, manager.queue, 1)
	terminal := <-manager.queue
	require.Contains(test, string(terminal.body), "final-thread")
	body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.analytics_enabled", false)
	require.Nil(test, beginCodexTelemetry(codexTelemetryRequest{account: account, body: body, headers: headers}))
	require.Empty(test, manager.queue)
	body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.analytics_enabled", true)
	body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.request_kind", "compaction")
	require.False(test, codexTelemetryEligible(body, headers))
	oldAttempt := &codexTelemetryAttempt{profile: attempt.profile}
	UpdateRuntimeSettings(func(settings RuntimeSettings) RuntimeSettings {
		settings.CodexTelemetryEnabled = false
		return settings
	})
	require.Empty(test, manager.metrics)
	UpdateRuntimeSettings(func(settings RuntimeSettings) RuntimeSettings { settings.CodexTelemetryEnabled = true; return settings })
	oldAttempt.finish("failed", nil)
	require.Empty(test, manager.queue)
}

func TestCodexTelemetryDropsDisabledAndStaleJobs(test *testing.T) {
	telemetryRuntimeForTest(test, false)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		writer.WriteHeader(http.StatusNoContent)
	}))
	test.Cleanup(server.Close)
	for _, enabled := range []bool{false, true} {
		UpdateRuntimeSettings(func(settings RuntimeSettings) RuntimeSettings {
			settings.CodexTelemetryEnabled = enabled
			return settings
		})
		manager := newCodexTelemetryManager()
		manager.generation.Store(1)
		manager.queue <- codexTelemetryJob{client: testCodexTelemetryProfile().client, url: server.URL, body: []byte(`{}`)}
		close(manager.queue)
		manager.worker()
	}
	require.Zero(test, calls.Load())
}

func TestCodexTelemetryDoesNotFollowRedirect(test *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { destinationCalls.Add(1) }))
	test.Cleanup(destination.Close)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, destination.URL, http.StatusTemporaryRedirect)
	}))
	test.Cleanup(server.Close)
	err := sendCodexTelemetryJob(codexTelemetryJob{client: testCodexTelemetryProfile().client, url: server.URL, body: []byte(`{}`)})
	require.Error(test, err)
	require.Zero(test, destinationCalls.Load())
}

func TestCodexAnalyticsDiagnosticKeepsBooleanWithLargeMetadata(test *testing.T) {
	metadata := `{"analytics_enabled":false,"tools":"` + strings.Repeat("private", 10000) + `"}`
	headers := http.Header{}
	headers.Set(codexTurnMetadataHeader, metadata)
	headerDiagnostic := CaptureOutboundIdentityHeaders(headers)
	require.Equal(test, gjson.False, gjson.Get(headerDiagnostic.Headers[codexTurnMetadataHeader], "analytics_enabled").Type)
	for _, objectCarrier := range []bool{true, false} {
		body := []byte(`{}`)
		if objectCarrier {
			body, _ = sjson.SetRawBytes(body, "client_metadata.x-codex-turn-metadata", []byte(metadata))
		} else {
			body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata", metadata)
		}
		encoded, err := json.Marshal(captureOutboundIdentityBody(body))
		require.NoError(test, err)
		require.Less(test, len(encoded), 512)
		require.NotContains(test, string(encoded), "private")
		require.Equal(test, gjson.False, codexTurnMetadata(encoded, nil).Get("analytics_enabled").Type)
	}
}
