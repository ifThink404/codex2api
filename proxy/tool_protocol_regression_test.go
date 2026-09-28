package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestChatToolNameValidationMatchesSupportedShapes(t *testing.T) {
	for _, tc := range []struct{ tool, path string }{
		{`{"function":{"name":""}}`, "tools[0].function.name"},
		{`{"type":null,"function":{"name":" "}}`, "tools[0].function.name"},
		{`{"type":"function","name":""}`, "tools[0].name"},
		{`{"type":"function"}`, "tools[0].name"},
		{`{"type":"namespace","name":"functions","tools":[{"type":"function","name":""}]}`, "tools[0].tools[0].name"},
		{`{"type":"custom","custom":{"name":""}}`, "tools[0].custom.name"},
	} {
		raw := []byte(fmt.Sprintf(`{"messages":[{"role":"user","content":"hello"}],"tools":[%s]}`, tc.tool))
		_, err := TranslateRequest(raw)
		require.ErrorContains(t, err, tc.path)
		_, err = TranslateChatToResponsesForGrok(raw)
		require.ErrorContains(t, err, tc.path)
	}
	for _, tool := range []string{
		`{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}`,
		`{"type":"function","name":"read_file","parameters":{"type":"object"}}`,
		`{"function":{"name":"read_file","parameters":{"type":"object"}}}`,
	} {
		raw := []byte(fmt.Sprintf(`{"model":"gpt-6-astra","messages":[{"role":"user","content":"hello"}],"tools":[%s]}`, tool))
		translated, err := TranslateRequest(raw)
		require.NoError(t, err)
		for _, profile := range []auth.CodexBPSProfile{auth.BPSWord, auth.BPSExcel, auth.BPSSheets, auth.BPSPowerPoint} {
			wire, _, err := prepareCodexBPSBodyForProfile(translated, "test", false, false, nil, bpsProfile(profile))
			require.NoError(t, err)
			require.Equal(t, "read_file", gjson.GetBytes(wire, "input.1.tools.0.name").String())
		}
	}
}

func TestResponsesAdditionalToolNameValidation(t *testing.T) {
	for _, kind := range []string{"additional_tools", "tool_search_output"} {
		body := []byte(fmt.Sprintf(`{"input":[{"type":%q,"tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":""}]}]}]}`, kind))
		require.ErrorContains(t, ValidateResponsesFunctionNames(body), "input[0].tools[0].tools[0].name")
	}
	require.NoError(t, ValidateResponsesFunctionNames([]byte(`{"tools":[{"type":"web_search"},{"type":"namespace","name":"functions","tools":[{"type":"function","name":"js"}]}]}`)))
}

func TestFunctionNamespaceChatRoundTrip(t *testing.T) {
	event := []byte(`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"js","namespace":"functions","arguments":"{}"}}`)
	translator := NewStreamTranslator("chat-1", "gpt-6-astra", 1)
	chunk, done := translator.TranslateParsed(gjson.ParseBytes(event))
	require.False(t, done)
	require.Equal(t, "functions", gjson.GetBytes(chunk, "choices.0.delta.tool_calls.0.namespace").String())
	completed := []byte(`{"response":{"output":[{"type":"function_call","call_id":"call_1","name":"js","namespace":"functions","arguments":"{}"}]}}`)
	calls, err := ExtractToolCallsFromOutputValidated(completed)
	require.NoError(t, err)
	response := BuildCompactResponse("chat-1", "gpt-6-astra", 1, "", "", calls, nil)
	message := gjson.GetBytes(response, "choices.0.message").Raw
	request := []byte(fmt.Sprintf(`{"model":"gpt-6-astra","messages":[%s,{"role":"tool","tool_call_id":"call_1","content":"private result"}]}`, message))
	body, err := TranslateRequest(request)
	require.NoError(t, err)
	require.Equal(t, "functions", gjson.GetBytes(body, "input.0.namespace").String())
	wire, _, err := prepareCodexBPSBodyForProfile(body, "test", false, false, nil, bpsProfile(auth.BPSExcel))
	require.NoError(t, err)
	require.Equal(t, "functions", gjson.GetBytes(wire, "input.1.namespace").String())
}

func TestToolProtocolDiagnosticsPrivacyBoundsAndNamespaces(t *testing.T) {
	body := []byte(`{"tools":[{"type":"namespace","name":"private-namespace","tools":[{"type":"function","name":"private-tool","description":"private-description","parameters":{"private-schema":true}}]}],"input":[{"type":"function_call","name":"private-tool","call_id":"private-id","arguments":"private-arguments"},{"type":"function_call_output","output":"private-result"}]}`)
	d := diagnoseToolProtocol(body)
	require.Equal(t, "array", d.TopLevelToolsType)
	require.Equal(t, 2, d.Declarations)
	require.Equal(t, 1, d.Calls)
	require.Equal(t, 1, d.MissingNamespaces)
	require.Equal(t, "input[0]", d.Issues[0].Path)
	require.Equal(t, "missing_namespace", d.Issues[0].Issue)
	encoded, err := json.Marshal(d)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "private-")
	// Default and namespaced functions may share a name; don't diagnose a valid
	// unqualified call as requiring a namespace.
	require.Zero(t, diagnoseToolProtocol([]byte(`{"tools":[{"type":"function","name":"js"},{"type":"namespace","name":"ns","tools":[{"type":"function","name":"js"}]}],"input":[{"type":"function_call","name":"js"}]}`)).MissingNamespaces)
	body = []byte(`{"tools":[` + strings.TrimSuffix(strings.Repeat(`{"type":"function","name":""},`, 50), ",") + `]}`)
	d = diagnoseToolProtocol(body)
	require.Equal(t, 50, d.MissingNames)
	require.Len(t, d.DeclarationSamples, 8)
	require.Len(t, d.Issues, 8)
	require.Equal(t, 42, d.OmittedIssues)
	encoded, err = json.Marshal(d)
	require.NoError(t, err)
	require.Less(t, len(encoded), 8192)
}

func TestToolProtocolIngressAndWireDiagnostics(t *testing.T) {
	c := transportTestContext()
	raw := []byte(`{"model":"gpt-6-astra","messages":[{"role":"user","content":"private-prompt"}],"tools":[{"function":{"name":"read_file","parameters":{"type":"object"}}}]}`)
	captureUsageRequestIngress(c, raw)
	translated, err := TranslateRequest(raw)
	require.NoError(t, err)
	wire, _, err := prepareCodexBPSBodyForProfile(translated, "test", false, false, nil, bpsProfile(auth.BPSExcel))
	require.NoError(t, err)
	beginUpstreamTrace(c.Request.Context(), &auth.Account{DBID: 3}, "", false)
	UpstreamTransportObserver(c.Request.Context()).ResponsesInput(wire, nil, "/responses")
	usage := &database.UsageLogInput{AccountID: 3, StatusCode: 400}
	populateUpstreamTrace(c, usage)
	populateUsageRequestDiagnostics(c, usage)
	incoming := gjson.Get(usage.RequestDiagnostics, "tool_protocol.declaration_samples.0")
	outgoing := gjson.Get(usage.RequestDiagnostics, "upstream.tool_protocol.declaration_samples.0")
	require.Equal(t, "absent", incoming.Get("name_state").String())
	require.Equal(t, "present", incoming.Get("nested_name_state").String())
	require.Equal(t, incoming.Get("nested_name_hash").String(), outgoing.Get("name_hash").String())
	require.Equal(t, "input[1].tools[0]", outgoing.Get("path").String())
	require.NotContains(t, usage.RequestDiagnostics, "private-prompt")
	beginUpstreamTrace(c.Request.Context(), &auth.Account{DBID: 4}, "", false)
	UpstreamTransportObserver(c.Request.Context()).ResponsesInput([]byte(`{"input":"hello"}`), nil, "/responses")
	require.Nil(t, snapshotUpstreamTrace(c.Request.Context()).Transport.ToolProtocol)
}

func TestServiceErrorSavesToolAndSelectionDiagnostics(t *testing.T) {
	h := newServiceErrorTestHandler(t)
	router := gin.New()
	router.Use(h.ServiceErrorMiddleware())
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		captureUsageRequestIngress(c, []byte(`{"messages":[],"tools":[{"type":"function","name":""}]}`))
		beginDispatchSelection(c)
		trace := selectionTraceForRequest(c)
		trace.Reset() // retry entry must retain detail capture
		trace.PinAccount(3)
		trace.Bind(3)
		trace.RejectAccount(3, "account_paused")
		api.SendErrorWithStatus(c, api.NewAPIError(api.ErrCodeServiceUnavailable, dispatchPublicMessage, api.ErrorTypeServer), 503)
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest("POST", "/v1/chat/completions", nil))
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.NotContains(t, recorder.Body.String(), "account_paused")
	page := serviceErrorTestPage(t, h)
	require.Len(t, page.Items, 1)
	event := page.Items[0]
	require.EqualValues(t, 3, gjson.GetBytes(event.DispatchSelection, "pinned_account_id").Int())
	require.EqualValues(t, 3, gjson.GetBytes(event.DispatchSelection, "candidates.samples.0.account_id").Int())
	require.Equal(t, "account_paused", gjson.GetBytes(event.DispatchSelection, "candidates.samples.0.reason").String())
	require.EqualValues(t, 1, gjson.GetBytes(event.ToolProtocol, "missing_names").Int())
}

func TestChatRejectsUnnamedToolBeforeAccountSelection(t *testing.T) {
	h := newServiceErrorTestHandler(t)
	router := gin.New()
	router.Use(h.ServiceErrorMiddleware())
	router.POST("/v1/chat/completions", h.ChatCompletions)
	recorder := httptest.NewRecorder()
	body := `{"model":"gpt-6-astra","messages":[{"role":"user","content":"private-prompt"}],"tools":[{"function":{"name":""}}]}`
	router.ServeHTTP(recorder, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "tools[0].function.name")
	page := serviceErrorTestPage(t, h)
	require.Len(t, page.Items, 1)
	require.EqualValues(t, 1, gjson.GetBytes(page.Items[0].ToolProtocol, "missing_names").Int())
	require.Equal(t, "not_started", gjson.GetBytes(page.Items[0].UpstreamInfo, "transport").String())
}

func TestResponsesInvalidToolsTypeIsSavedWithoutContents(t *testing.T) {
	for _, tc := range []struct{ name, value, kind string }{
		{"null", `null`, "null"},
		{"object", `{"private-key":{"type":"function","name":"private-tool"}}`, "object"},
		{"string", `"[{\"type\":\"function\",\"name\":\"private-tool\"}]"`, "string"},
		{"true", `true`, "boolean"},
		{"false", `false`, "boolean"},
		{"number", `1`, "number"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newServiceErrorTestHandler(t)
			router := gin.New()
			router.Use(h.ServiceErrorMiddleware())
			router.POST("/v1/responses", h.Responses)
			body := `{"model":"gpt-6-astra","input":"private-prompt","tools":` + tc.value + `}`
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))
			require.Equal(t, http.StatusBadRequest, recorder.Code)
			require.Contains(t, recorder.Body.String(), "Field 'tools' must be an array")
			page := serviceErrorTestPage(t, h)
			require.Len(t, page.Items, 1)
			event := page.Items[0]
			require.Equal(t, "validation", event.Stage)
			require.Equal(t, tc.kind, gjson.GetBytes(event.ToolProtocol, "top_level_tools_type").String())
			require.Zero(t, gjson.GetBytes(event.ToolProtocol, "declarations").Int())
			require.Equal(t, "not_started", gjson.GetBytes(event.UpstreamInfo, "transport").String())
			encoded, err := json.Marshal(event)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "private-")
		})
	}
	for _, fields := range []string{"", `,"tools":[]`} {
		body := []byte(`{"model":"gpt-6-astra","input":"hello"` + fields + `}`)
		require.True(t, api.ValidateResponsesAPIRequest(body, []string{"gpt-6-astra"}).Valid)
		require.Nil(t, diagnoseToolProtocol(body), "ordinary tool-free requests should not add diagnostics")
	}
	for _, tc := range []struct{ fields, kind string }{
		{"", "absent"}, {`,"tools":[]`, "array"}, {`,"tools":null`, "null"},
	} {
		body := []byte(`{"input":[{"type":"custom_tool_call","name":"private-tool","call_id":"private-call","input":"private-input"}]` + tc.fields + `}`)
		d := diagnoseToolProtocol(body)
		require.NotNil(t, d)
		require.Equal(t, tc.kind, d.TopLevelToolsType)
		require.Equal(t, 1, d.Calls)
		require.Zero(t, d.Declarations)
	}
}
