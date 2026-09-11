package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

func testCodexTelemetryProfile() codexTelemetryProfile {
	account := &auth.Account{DBID: 7, AccessToken: "test-token", AccountID: "acct-test"}
	return codexTelemetryProfile{
		client: codexTelemetryClient{
			account: account, accessToken: account.AccessToken, accountID: account.AccountID,
			userAgent:  "codex-tui/0.153.4 (Mac OS 15.5.0; arm64) xterm-256color (codex-tui; 0.153.4)",
			originator: "codex_cli_rs", version: "0.153.4",
		},
		sessionID: "session-1", threadID: "thread-1", turnID: "turn-1", rootTurnID: "turn-1",
		model: "gpt-6-astra", effort: "high", serviceTier: "default", started: time.Now().Add(-time.Second),
		firstThread: true, dynamicTool: true, command: true, fileChange: true,
	}
}

func TestCodexTelemetryEventContract(t *testing.T) {
	profile := testCodexTelemetryProfile()
	terminal := []byte(`{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":12,"output_tokens":8,"total_tokens":20}}}`)
	events := append(codexInitializationEvents(profile), codexTerminalEvents(profile, codexTelemetryTerminal{status: "completed", body: terminal})...)
	counts := make(map[string]int)
	var mainTurn, accepted map[string]any
	for _, event := range events {
		counts[event.EventType]++
		if event.EventType == "codex_turn_steer_event" {
			t.Fatal("Responses telemetry must not infer native turn/steer RPCs")
		}
		if event.EventType == "codex_turn_event" && event.EventParams["thread_source"] == "user" {
			mainTurn = event.EventParams
		}
		if event.EventType == "codex_turn_event" && event.EventParams["thread_source"] == "thread_title" {
			if event.EventParams["root_turn_id"] != event.EventParams["turn_id"] || event.EventParams["total_tool_call_count"] != 0 {
				t.Fatalf("title turn params = %#v", event.EventParams)
			}
		}
		if event.EventType == "codex_thread_initialized" && event.EventParams["thread_source"] == "guardian_review" {
			if event.EventParams["parent_thread_id"] != profile.threadID || event.EventParams["subagent_source"] != "guardian" || event.EventParams["ephemeral"] != false {
				t.Fatalf("guardian thread params = %#v", event.EventParams)
			}
		}
		if event.EventType == "codex_accepted_line_fingerprints" {
			accepted = event.EventParams
		}
	}
	if counts["codex_thread_initialized"] != 3 || counts["codex_turn_event"] != 2 || counts["codex_hook_run"] != 4 {
		t.Fatalf("event counts = %#v", counts)
	}
	if counts["codex_dynamic_tool_call_event"] != 1 || counts["codex_command_execution_event"] != 1 || counts["codex_file_change_event"] != 1 || counts["codex_accepted_line_fingerprints"] != 1 {
		t.Fatalf("random event counts = %#v", counts)
	}
	if mainTurn["initialization_mode"] != "new" || mainTurn["steer_count"] != 0 || mainTurn["total_tokens"] != int64(20) {
		t.Fatalf("main turn params = %#v", mainTurn)
	}
	if accepted["repo_hash"] != nil || len(accepted["line_fingerprints"].([]any)) != 0 {
		t.Fatalf("accepted fingerprint params = %#v", accepted)
	}
}

func TestCodexTelemetryDoesNotInferResume(t *testing.T) {
	profile := testCodexTelemetryProfile()
	body := []byte(`{"model":"gpt-6-astra","previous_response_id":"resp_old","input":[{"role":"assistant"},{"type":"function_call_output"}]}`)
	profile = buildCodexTelemetryProfile(profile.client, codexTelemetryRequest{account: profile.client.account, body: body, sessionID: "session-1", headers: http.Header{}})
	params := codexMainTurnEvent(profile, codexTelemetryTerminal{status: "completed"}).EventParams
	if params["initialization_mode"] != "new" || params["steer_count"] != 0 {
		t.Fatalf("history changed turn classification: %#v", params)
	}
}

func TestCodexTelemetryMetricsContract(t *testing.T) {
	if len(codexMetricDescriptors) != 66 {
		t.Fatalf("metric descriptor count = %d, want 66", len(codexMetricDescriptors))
	}
	names := make(map[string]bool, len(codexMetricDescriptors))
	samples := make([]codexMetricSample, 0, len(codexMetricDescriptors))
	for _, descriptor := range codexMetricDescriptors {
		if names[descriptor.name] {
			t.Fatalf("duplicate metric %q", descriptor.name)
		}
		names[descriptor.name] = true
		samples = append(samples, codexMetricSample{descriptor: descriptor, value: 1})
	}
	for _, name := range []string{"codex.hooks.run", "codex.hooks.run.duration_ms", "codex.external_agent_config.detect", "codex.rollout.size_bytes"} {
		if !names[name] {
			t.Fatalf("missing dynamic metric %q", name)
		}
	}
	var payload map[string]any
	if err := json.Unmarshal(buildCodexMetricsPayload(testCodexTelemetryProfile(), time.Now(), samples), &payload); err != nil {
		t.Fatalf("decode OTLP payload: %v", err)
	}
	resourceMetrics := payload["resourceMetrics"].([]any)
	scopeMetrics := resourceMetrics[0].(map[string]any)["scopeMetrics"].([]any)
	metrics := scopeMetrics[0].(map[string]any)["metrics"].([]any)
	if len(metrics) != 66 {
		t.Fatalf("OTLP metric count = %d, want 66", len(metrics))
	}
	for _, raw := range metrics {
		metric := raw.(map[string]any)
		for _, kind := range []string{"sum", "histogram"} {
			if aggregation, ok := metric[kind].(map[string]any); ok && aggregation["aggregationTemporality"] != float64(1) {
				t.Fatalf("metric %q is not delta temporality", metric["name"])
			}
		}
		if metric["name"] == "codex.hooks.run.duration_ms" {
			points := metric["histogram"].(map[string]any)["dataPoints"].([]any)
			if points[0].(map[string]any)["count"] != float64(4) {
				t.Fatal("hook duration histogram must contain four hook observations")
			}
		}
	}
}

func TestCodexDesktopMetricIdentity(t *testing.T) {
	profile := testCodexTelemetryProfile()
	profile.client.userAgent = "Codex Desktop/0.153.4 (Windows 10.0.26200; x86_64) unknown (Codex Desktop; 26.903.61454)"
	profile.client.originator = "Codex Desktop"
	if got := codexMetricAttributeValue(profile, "codex.turn.e2e_duration_ms", "originator"); got != "Codex_Desktop" {
		t.Fatalf("metric originator = %q", got)
	}
	if got := codexMetricAttributeValue(profile, "codex.turn.e2e_duration_ms", "service_name"); got != "codex_desktop" {
		t.Fatalf("metric service name = %q", got)
	}
	if got := codexMetricAttributeValue(profile, "codex.process.start", "originator"); got != "codex-app-server" {
		t.Fatalf("process originator = %q", got)
	}
}

func TestCodexTelemetryTransportHeaders(t *testing.T) {
	t.Setenv("CODEX_STATSIG_API_KEY", "test-statsig-key")
	requests := make(chan http.Header, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		requests <- request.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	profile := testCodexTelemetryProfile()
	jobs := []codexTelemetryJob{
		{client: profile.client, url: server.URL, body: []byte(`{}`)},
		{client: profile.client, url: server.URL, body: []byte(`{}`), metrics: true},
	}
	for _, job := range jobs {
		if err := sendCodexTelemetryJob(job); err != nil {
			t.Fatalf("send telemetry: %v", err)
		}
	}
	analytics, metrics := <-requests, <-requests
	if analytics.Get("Authorization") != "Bearer test-token" || analytics.Get("Chatgpt-Account-Id") != "acct-test" || analytics.Get("Originator") != "codex_cli_rs" {
		t.Fatalf("analytics headers = %#v", analytics)
	}
	if metrics.Get("Authorization") != "" || metrics.Get("statsig-api-key") != "test-statsig-key" || metrics.Get("User-Agent") != "OTel-OTLP-Exporter-Rust/0.31.0" {
		t.Fatalf("metrics headers = %#v", metrics)
	}
}
