package proxy

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"
)

type codexMetricDescriptor struct {
	name       string
	kind       string
	unit       string
	attributes string
}

type codexMetricSample struct {
	descriptor codexMetricDescriptor
	value      float64
}

type codexMetricState struct {
	profile           codexTelemetryProfile
	started           time.Time
	lastSeen          time.Time
	pending           map[string]float64
	externalAgentSent bool
}

var codexMetricDescriptors = []codexMetricDescriptor{
	{"codex.process.start", "sum", "", "originator"},
	{"codex.sqlite.init.count", "sum", "", "db,error,originator,phase,status"},
	{"codex.sqlite.init.duration_ms", "histogram", "ms", "db,error,originator,phase,status"},
	{"codex.app_server.codex_home.size_bytes", "histogram", "", "compression_enabled,directory"},
	{"codex.remote_models.fetch_update.duration_ms", "histogram", "ms", ""},
	{"codex.plugins.loaded_cache.event", "sum", "", "event"},
	{"codex.remote_models.load_cache.duration_ms", "histogram", "ms", ""},
	{"codex.plugins.loaded_cache.wait.duration_ms", "histogram", "ms", ""},
	{"codex.plugins.loaded_cache.load.duration_ms", "histogram", "ms", ""},
	{"codex.plugins.loaded_cache.request", "sum", "", "outcome"},
	{"codex.mcp.protocol_discovery", "sum", "", "mode,outcome,server_kind"},
	{"codex.mcp.protocol_discovery.duration_ms", "histogram", "ms", "mode,outcome,server_kind"},
	{"codex.mcp.tools.fetch_uncached.duration_ms", "histogram", "ms", "trigger"},
	{"codex.mcp.tools.list.duration_ms", "histogram", "ms", "cache"},
	{"codex.apps.installed.duration_ms", "histogram", "ms", "force_refresh,outcome,path,refresh,reload,retained_previous_snapshot"},
	{"codex.apps.installed.response_bytes", "histogram", "", "path"},
	{"codex.apps.installed.connector_count", "histogram", "", "path"},
	{"codex.apps.installed.tool_count", "histogram", "", "path"},
	{"codex.apps.snapshot.age_ms", "histogram", "ms", "observation,path"},
	{"codex.apps.read.duration_ms", "histogram", "ms", "include_tools"},
	{"codex.sqlite.logs.write.count", "sum", "", "error,originator,status"},
	{"codex.sqlite.logs.write.duration_ms", "histogram", "ms", "error,originator,status"},
	{"codex.sqlite.logs.write.bytes", "histogram", "", "error,originator,status"},
	{"codex.sqlite.logs.write.entries", "histogram", "", "error,originator,status"},
	{"codex.sqlite.logs.write.max_entry_bytes", "histogram", "", "error,originator,status"},
	{"codex.mcp.tools.cache_write.duration_ms", "histogram", "ms", "status"},
	{"codex.mcp.tools.cache_publish.duration_ms", "histogram", "ms", "result,source"},
	{"codex.apps.refresh.duration_ms", "histogram", "ms", "path,trigger"},
	{"codex.feature.state", "sum", "", "app.version,auth_mode,feature,model,originator,service_name,session_source,value"},
	{"codex.thread.started", "sum", "", "app.version,auth_mode,is_git,model,originator,service_name,session_source"},
	{"codex.shell_snapshot.duration_ms", "histogram", "ms", "app.version,auth_mode,model,originator,session_source,success,version"},
	{"codex.shell_snapshot", "sum", "", "app.version,auth_mode,failure_reason,model,originator,session_source,success,version"},
	{"codex.startup.phase.duration_ms", "histogram", "ms", "app.version,auth_mode,model,originator,phase,service_name,session_source,status"},
	{"codex.websocket.request", "sum", "", "app.version,auth_mode,model,originator,service_name,session_source,success"},
	{"codex.websocket.request.duration_ms", "histogram", "ms", "app.version,auth_mode,model,originator,service_name,session_source,success"},
	{"codex.rollout_compression.materialize", "sum", "", "outcome"},
	{"codex.websocket.event", "sum", "", "app.version,auth_mode,kind,model,originator,service_name,session_source,success"},
	{"codex.websocket.event.duration_ms", "histogram", "ms", "app.version,auth_mode,kind,model,originator,service_name,session_source,success"},
	{"codex.startup_prewarm.duration_ms", "histogram", "ms", "app.version,auth_mode,model,originator,service_name,session_source,status"},
	{"codex.startup_prewarm.age_at_first_turn_ms", "histogram", "ms", "app.version,auth_mode,model,originator,service_name,session_source,status"},
	{"codex.thread.skills.enabled_total", "histogram", "", "app.version,auth_mode,catalog_surface,model,originator,service_name,session_source"},
	{"codex.thread.skills.kept_total", "histogram", "", "app.version,auth_mode,catalog_surface,model,originator,service_name,session_source"},
	{"codex.thread.skills.truncated", "histogram", "", "app.version,auth_mode,catalog_surface,model,originator,service_name,session_source"},
	{"codex.thread.skills.description_truncated_chars", "histogram", "", "app.version,auth_mode,catalog_surface,model,originator,service_name,session_source"},
	{"codex.skills.shadow_selection", "sum", "", "candidate_set_truncated,method,query_script,query_truncated,status"},
	{"codex.skills.shadow_selection.duration_ms", "histogram", "ms", "candidate_set_truncated,method,query_script,query_truncated,status"},
	{"codex.skills.shadow_selection.catalog_entries", "histogram", "", "candidate_set_truncated,method,query_script,query_truncated,status"},
	{"codex.skills.shadow_selection.selected_entries", "histogram", "", "candidate_set_truncated,method,query_script,query_truncated,status"},
	{"codex.skills.shadow_selection.query_terms", "histogram", "", "candidate_set_truncated,method,query_script,query_truncated,status"},
	{"codex.skills.shadow_selection.reduction_bps", "histogram", "", "candidate_set_truncated,method,query_script,query_truncated,status"},
	{"codex.turn.ttft.duration_ms", "histogram", "ms", "app.version,auth_mode,model,originator,service_name,session_source"},
	{"codex.turn.ttfm.duration_ms", "histogram", "ms", "app.version,auth_mode,model,originator,service_name,session_source"},
	{"codex.responses_api_overhead.duration_ms", "histogram", "ms", "app.version,auth_mode,model,originator,service_name,session_source"},
	{"codex.responses_api_inference_time.duration_ms", "histogram", "ms", "app.version,auth_mode,model,originator,service_name,session_source"},
	{"codex.responses_api_engine_iapi_tbt.duration_ms", "histogram", "ms", "app.version,auth_mode,model,originator,service_name,session_source"},
	{"codex.turn.e2e_duration_ms", "histogram", "ms", "app.version,auth_mode,model,originator,service_name,session_source"},
	{"codex.turn.network_proxy", "sum", "", "active,app.version,auth_mode,model,originator,service_name,session_source,tmp_mem_enabled"},
	{"codex.turn.tool.call", "histogram", "", "app.version,auth_mode,model,originator,service_name,session_source,tmp_mem_enabled"},
	{"codex.turn.memory", "sum", "", "app.version,auth_mode,config_use_memories,feature_enabled,has_citations,model,originator,read_allowed,service_name,session_source"},
	{"codex.turn.unified_exec.running_processes", "sum", "", "app.version,auth_mode,model,originator,service_name,session_source"},
	{"codex.windows_mxc.available", "sum", "", "available"},
	{"codex.tool.unified_exec", "sum", "", "app.version,auth_mode,model,originator,session_source,tty"},
	{"codex.hooks.run", "sum", "", "app.version,auth_mode,execution_mode,handler_type,hook_name,model,originator,session_source,source,status"},
	{"codex.hooks.run.duration_ms", "histogram", "ms", "app.version,auth_mode,execution_mode,handler_type,hook_name,model,originator,session_source,source,status"},
	{"codex.external_agent_config.detect", "sum", "", "migration_type"},
	{"codex.rollout.size_bytes", "histogram", "", ""},
}

func (m *codexTelemetryManager) touchMetrics(profile codexTelemetryProfile) {
	accountID := profile.client.account.ID()
	m.mu.Lock()
	state := m.metrics[accountID]
	if state != nil {
		state.profile, state.lastSeen = profile, profile.started
		m.mu.Unlock()
		return
	}
	state = &codexMetricState{profile: profile, started: profile.started, lastSeen: profile.started, pending: make(map[string]float64)}
	m.metrics[accountID] = state
	m.mu.Unlock()
	samples := make([]codexMetricSample, 0, 62)
	for _, descriptor := range codexMetricDescriptors[:62] {
		samples = append(samples, codexMetricSample{descriptor: descriptor, value: codexStartupMetricValue(profile, descriptor)})
	}
	m.enqueueMetrics(profile.client, buildCodexMetricsPayload(profile, state.started, samples))
}

func (m *codexTelemetryManager) recordTurnMetrics(profile codexTelemetryProfile, result codexTelemetryTerminal) {
	now := time.Now()
	m.mu.Lock()
	state := m.metrics[profile.client.account.ID()]
	if state == nil {
		m.mu.Unlock()
		m.touchMetrics(profile)
		m.mu.Lock()
		state = m.metrics[profile.client.account.ID()]
	}
	state.profile, state.lastSeen = profile, now
	state.pending["codex.turn.e2e_duration_ms"] += float64(max(now.Sub(profile.started).Milliseconds(), 0))
	state.pending["codex.turn.ttft.duration_ms"] += float64(elapsedMillis(profile.started, result.firstEvent, now))
	state.pending["codex.turn.ttfm.duration_ms"] += float64(elapsedMillis(profile.started, result.firstToken, now))
	state.pending["codex.hooks.run"], state.pending["codex.hooks.run.duration_ms"] = state.pending["codex.hooks.run"]+4, state.pending["codex.hooks.run.duration_ms"]+4
	state.pending["codex.turn.tool.call"] += float64(boolInt(profile.dynamicTool) + boolInt(profile.fileChange))
	if profile.command {
		state.pending["codex.tool.unified_exec"]++
	}
	if profile.fileChange {
		state.pending["codex.rollout.size_bytes"] += float64(1024 + simulatedInt(profile.turnID+":rollout", 196608))
	}
	if !state.externalAgentSent {
		state.pending["codex.external_agent_config.detect"]++
		state.externalAgentSent = true
	}
	m.mu.Unlock()
}

func (m *codexTelemetryManager) flushMetrics(now time.Time) {
	type batch struct {
		client  codexTelemetryClient
		profile codexTelemetryProfile
		started time.Time
		values  map[string]float64
	}
	m.mu.Lock()
	batches := make([]batch, 0, len(m.metrics))
	for accountID, state := range m.metrics {
		if len(state.pending) > 0 {
			batches = append(batches, batch{state.profile.client, state.profile, state.started, state.pending})
			state.pending = make(map[string]float64)
		}
		if now.Sub(state.lastSeen) > codexTelemetryStateTTL {
			delete(m.metrics, accountID)
		}
	}
	for key, seen := range m.threads {
		if now.Sub(seen) > codexTelemetryStateTTL {
			delete(m.threads, key)
		}
	}
	m.mu.Unlock()
	for _, item := range batches {
		m.enqueueMetrics(item.client, buildCodexMetricsPayload(item.profile, item.started, codexMetricSamples(item.values)))
	}
}

func codexMetricSamples(values map[string]float64) []codexMetricSample {
	samples := make([]codexMetricSample, 0, len(values))
	for _, descriptor := range codexMetricDescriptors {
		if value, ok := values[descriptor.name]; ok {
			samples = append(samples, codexMetricSample{descriptor: descriptor, value: value})
		}
	}
	return samples
}

func (m *codexTelemetryManager) enqueueMetrics(client codexTelemetryClient, body []byte) {
	if len(body) > 0 {
		m.enqueue(codexTelemetryJob{client: client, url: codexMetricsEndpoint, body: body, metrics: true})
	}
}

func buildCodexMetricsPayload(profile codexTelemetryProfile, started time.Time, samples []codexMetricSample) []byte {
	metrics := make([]any, 0, len(samples))
	for _, sample := range samples {
		metrics = append(metrics, codexOTLPMetric(profile, started, sample))
	}
	resource := map[string]any{"attributes": codexResourceAttributes(profile), "droppedAttributesCount": 0, "entityRefs": []any{}}
	scope := map[string]any{"name": "codex", "version": "", "attributes": []any{}, "droppedAttributesCount": 0}
	scopeMetrics := map[string]any{"scope": scope, "metrics": metrics, "schemaUrl": ""}
	payload := map[string]any{"resourceMetrics": []any{map[string]any{"resource": resource, "scopeMetrics": []any{scopeMetrics}, "schemaUrl": ""}}}
	body, _ := json.Marshal(payload)
	return body
}

func codexOTLPMetric(profile codexTelemetryProfile, started time.Time, sample codexMetricSample) map[string]any {
	nowNanos := strconv.FormatInt(time.Now().UnixNano(), 10)
	startNanos := strconv.FormatInt(started.UnixNano(), 10)
	point := map[string]any{
		"attributes": codexMetricAttributes(profile, sample.descriptor), "startTimeUnixNano": startNanos,
		"timeUnixNano": nowNanos, "exemplars": []any{}, "flags": 0,
	}
	metric := map[string]any{"name": sample.descriptor.name, "description": "", "unit": sample.descriptor.unit, "metadata": []any{}}
	if sample.descriptor.kind == "sum" {
		point["asInt"] = int64(sample.value)
		metric["sum"] = map[string]any{"dataPoints": []any{point}, "aggregationTemporality": 1, "isMonotonic": true}
		return metric
	}
	if sample.descriptor.unit == "ms" {
		metric["description"] = "Duration in milliseconds."
	}
	count, minimum, maximum := uint64(1), sample.value, sample.value
	if sample.descriptor.name == "codex.hooks.run.duration_ms" {
		count, minimum, maximum = 4, 0, max(sample.value/4, 1)
	}
	point["count"], point["sum"], point["min"], point["max"] = count, sample.value, minimum, maximum
	point["explicitBounds"] = codexHistogramBounds
	buckets := codexHistogramBuckets(sample.value/float64(count), count)
	if sample.descriptor.name == "codex.hooks.run.duration_ms" {
		buckets[0], buckets[1] = 1, count-1
	}
	point["bucketCounts"] = buckets
	metric["histogram"] = map[string]any{"dataPoints": []any{point}, "aggregationTemporality": 1}
	return metric
}

var codexHistogramBounds = []float64{0, 5, 10, 25, 50, 75, 100, 250, 500, 750, 1000, 1250, 1500, 1750, 2000, 2250, 2500, 3000, 3500, 4000, 4500, 5000, 6000, 7000, 7500, 8000, 9000, 10000, 12000, 15000, 20000, 30000, 60000, 120000}

func codexHistogramBuckets(value float64, count uint64) []uint64 {
	buckets := make([]uint64, len(codexHistogramBounds)+1)
	index := len(codexHistogramBounds)
	for i, bound := range codexHistogramBounds {
		if value <= bound {
			index = i
			break
		}
	}
	buckets[index] = count
	return buckets
}

func codexResourceAttributes(profile codexTelemetryProfile) []any {
	_, _, osName, osVersion, _ := codexUserAgentParts(profile.client.userAgent, profile.client.version)
	return codexOTLPAttributes(map[string]string{
		"os": osName, "os_version": osVersion, "service.version": profile.client.version, "env": "dev",
		"telemetry.sdk.version": "0.31.0", "telemetry.sdk.language": "rust",
		"service.name": codexMetricResourceService(profile), "telemetry.sdk.name": "opentelemetry",
	})
}

func codexMetricAttributes(profile codexTelemetryProfile, descriptor codexMetricDescriptor) []any {
	if descriptor.attributes == "" {
		return []any{}
	}
	values := make(map[string]string)
	for _, name := range strings.Split(descriptor.attributes, ",") {
		if value := codexMetricAttributeValue(profile, descriptor.name, name); value != "" {
			values[name] = value
		}
	}
	return codexOTLPAttributes(values)
}

func codexOTLPAttributes(values map[string]string) []any {
	attributes := make([]any, 0, len(values))
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := values[key]
		attributes = append(attributes, map[string]any{"key": key, "value": map[string]any{"stringValue": value}})
	}
	return attributes
}

func codexMetricAttributeValue(profile codexTelemetryProfile, metric, name string) string {
	if name == "originator" && (metric == "codex.process.start" || strings.HasPrefix(metric, "codex.sqlite.")) {
		return codexMetricResourceService(profile)
	}
	values := map[string]string{
		"app.version": profile.client.version, "auth_mode": "Chatgpt", "model": profile.model,
		"originator": codexMetricOriginator(profile), "service_name": codexMetricProductService(profile),
		"session_source": codexMetricSessionSource(profile), "status": "success", "success": "true",
		"error": "none", "outcome": "success", "active": "false", "available": "true",
		"is_git": "false", "tty": "false", "cache": "miss", "compression_enabled": "false",
		"execution_mode": "sync", "handler_type": "mcp_tool", "hook_name": "Stop", "source": "plugin",
		"migration_type": "config", "tmp_mem_enabled": "false", "value": "true",
		"candidate_set_truncated": "false", "catalog_surface": "thread_context", "config_use_memories": "true",
		"db": "state", "directory": "codex_home", "event": "clear", "feature": "hooks",
		"feature_enabled": "false", "failure_reason": "write_failed", "force_refresh": "false",
		"has_citations": "false", "include_tools": "false", "kind": "response.completed",
		"method": "weighted_lexical_v1", "mode": "legacy", "observation": "installed",
		"path": "new", "phase": "open_state", "query_script": "mixed", "query_truncated": "false",
		"read_allowed": "false", "refresh": "not_requested", "reload": "false", "result": "published",
		"retained_previous_snapshot": "false", "server_kind": "openai_codex_apps", "trigger": "initial", "version": "v1",
	}
	if value := values[name]; value != "" {
		return value
	}
	return "default"
}

func codexMetricResourceService(profile codexTelemetryProfile) string {
	if strings.EqualFold(codexClientName(profile), "Codex Desktop") {
		return "codex-app-server"
	}
	return firstNonEmptyString(profile.client.originator, "codex_cli_rs")
}

func codexMetricOriginator(profile codexTelemetryProfile) string {
	value := strings.Map(func(char rune) rune {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("._-/", char) {
			return char
		}
		return '_'
	}, profile.client.originator)
	value = strings.Trim(value, "_")
	if value == "" {
		return "unspecified"
	}
	if len(value) > 256 {
		return value[:256]
	}
	return value
}

func codexMetricProductService(profile codexTelemetryProfile) string {
	name := strings.ToLower(codexClientName(profile))
	if name == "codex desktop" {
		return "codex_desktop"
	}
	if name == "codex_vscode" {
		return "codex_vscode"
	}
	return ""
}

func codexMetricSessionSource(profile codexTelemetryProfile) string {
	if service := codexMetricProductService(profile); service != "" {
		return "vscode"
	}
	return "cli"
}

func codexStartupMetricValue(profile codexTelemetryProfile, descriptor codexMetricDescriptor) float64 {
	if descriptor.name == "codex.turn.unified_exec.running_processes" || descriptor.name == "codex.turn.tool.call" {
		return 0
	}
	if descriptor.name == "codex.windows_mxc.available" && !strings.EqualFold(codexRuntime(profile)["runtime_os"].(string), "windows") {
		return 0
	}
	if descriptor.kind == "sum" {
		return 1
	}
	limit := 64
	if descriptor.unit == "ms" {
		limit = 4000
	} else if strings.Contains(descriptor.name, "bytes") {
		limit = 262144
	}
	return float64(simulatedInt(profile.turnID+":"+descriptor.name, limit))
}
