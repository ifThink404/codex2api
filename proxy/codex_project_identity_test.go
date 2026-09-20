package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const projectTestID = "01a07f21-24a6-7ee2-b095-f0f5fdaee3d3"
const unrelatedProjectUUID = "01a08479-2b74-7903-878c-72976cc66e5c"

func projectTestRequest(t *testing.T, h *Handler, account *auth.Account, user int64, body string) (context.Context, []byte, string) {
	t.Helper()
	c, _, _ := responsePrivacyRequest(t, h, user, "turn", "")
	ctx, out, err := PrepareCodexProjectOutbound(c.Request.Context(), account, []byte(body), nil)
	require.NoError(t, err)
	s := projectIdentityFrom(ctx)
	require.NotNil(t, s)
	return ctx, out, s.forward[projectTestID]
}

func TestProjectIdentityBusinessRoundTripAndIsolation(t *testing.T) {
	h, account, other, _ := responsePrivacySetup(t)
	nested, _ := json.Marshal(map[string]any{"project": map[string]string{"id": projectTestID}, "nested": fmt.Sprintf(`{"project_id":%q}`, projectTestID)})
	body, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "client_metadata": map[string]string{"project_id": projectTestID}, "input": []any{
		map[string]any{"role": "user", "content": []any{map[string]string{"type": "input_text", "text": "Use " + projectTestID + " and " + unrelatedProjectUUID}}},
		map[string]any{"type": "function_call_output", "call_id": "call_keep", "output": string(nested)},
		map[string]string{"type": "reasoning", "encrypted_content": projectTestID},
	}})
	ctx, out, b := projectTestRequest(t, h, account, 101, string(body))
	require.NotEmpty(t, b)
	require.Contains(t, gjson.GetBytes(out, "input.0.content.0.text").String(), b)
	require.Contains(t, string(out), unrelatedProjectUUID)
	require.NotContains(t, gjson.GetBytes(out, "input.1.output").String(), projectTestID)
	require.Equal(t, projectTestID, gjson.GetBytes(out, "input.2.encrypted_content").String())
	opaqueJSON, _ := json.Marshal(map[string]string{"project_id": projectTestID, "encrypted_content": projectTestID, "signature": projectTestID})
	opaqueBody, _ := json.Marshal(map[string]string{"input": string(opaqueJSON)})
	_, opaqueOut, _ := projectTestRequest(t, h, account, 101, string(opaqueBody))
	inner := gjson.GetBytes(opaqueOut, "input").String()
	require.Equal(t, projectTestID, gjson.Get(inner, "encrypted_content").String())
	require.Equal(t, projectTestID, gjson.Get(inner, "signature").String())
	require.Equal(t, b, gjson.Get(inner, "project_id").String())
	_, again, err := PrepareCodexProjectOutbound(ctx, account, out, nil)
	require.NoError(t, err)
	require.Equal(t, out, again)
	echo, _ := json.Marshal(map[string]any{"output": []any{map[string]any{"type": "function_call", "name": "use_project", "arguments": fmt.Sprintf(`{"projectId":%q}`, b)}}})
	restored, err := maskResponsePayload(ctx, account, echo, true)
	require.NoError(t, err)
	require.Contains(t, string(restored), projectTestID)
	require.NotContains(t, string(restored), b)
	_, next, b2 := projectTestRequest(t, h, account, 101, `{"input":"`+projectTestID+`"}`)
	require.Equal(t, b, b2)
	require.Contains(t, string(next), b)
	_, replay, same := projectTestRequest(t, h, account, 101, `{"input":"project_id: `+b+`"}`)
	require.Equal(t, b, same)
	require.Contains(t, string(replay), b)
	known, err := h.db.IsKnownCodexProjectID(t.Context(), projectIdentityFrom(ctx).binding.Scope, b)
	require.NoError(t, err)
	require.False(t, known)
	_, otherBody, b3 := projectTestRequest(t, h, other, 101, `{"input":"`+projectTestID+`"}`)
	require.NotEmpty(t, b3)
	require.NotEqual(t, b, b3)
	require.Contains(t, string(otherBody), b3)
	_, different, b4 := projectTestRequest(t, h, account, 102, `{"input":"`+projectTestID+`"}`)
	require.Empty(t, b4)
	require.Contains(t, string(different), projectTestID)
	c, _, _ := responsePrivacyRequest(t, h, 101, "later", "")
	later, _, err := PrepareCodexProjectOutbound(c.Request.Context(), account, []byte(`{"input":"continue"}`), nil)
	require.NoError(t, err)
	restored, err = maskResponsePayload(later, account, echo, true)
	require.NoError(t, err)
	require.Contains(t, string(restored), projectTestID)
}

func TestProjectIdentityDiscoveryAndExactReplacement(t *testing.T) {
	h, account, _, _ := responsePrivacySetup(t)
	for _, text := range []string{`{"project_id":"` + projectTestID + `"}`, `{"project":{"id":"` + projectTestID + `"}}`, "项目 ID：`" + projectTestID + "`", `{"project_id":"\u0030` + projectTestID[1:] + `"}`} {
		body, _ := json.Marshal(map[string]any{"input": text})
		_, out, b := projectTestRequest(t, h, account, 101, string(body))
		require.NotEmpty(t, b)
		require.Contains(t, string(out), b)
	}
	_, out, b := projectTestRequest(t, h, account, 101, `{"input":"prefix`+projectTestID+`suffix"}`)
	require.NotEmpty(t, b)
	require.Contains(t, string(out), "prefix"+b+"suffix")
	_, keys, _ := projectTestRequest(t, h, account, 101, `{"input":[{"output":{"`+projectTestID+`":"value","number":9007199254740993}}]}`)
	require.Contains(t, string(keys), `"`+b+`":"value"`)
	require.Contains(t, string(keys), "9007199254740993")
	_, extras, _ := projectTestRequest(t, h, account, 101, `{"metadata":{"business_project":"`+projectTestID+`"},"text":{"format":{"schema":{"enum":["`+projectTestID+`"]}}},"prompt":{"id":"pmpt_keep","variables":{"project":"`+projectTestID+`"}}}`)
	require.NotContains(t, string(extras), projectTestID)
	require.Equal(t, "pmpt_keep", gjson.GetBytes(extras, "prompt.id").String())
	c, _, _ := responsePrivacyRequest(t, h, 101, "turn", "")
	_, _, err := PrepareCodexProjectOutbound(c.Request.Context(), account, []byte(`{"input":{"`+projectTestID+`":1,"`+b+`":2}}`), nil)
	require.Error(t, err)
	deep := `"` + projectTestID + `"`
	for range 70 {
		deep = `{"nested":` + deep + `}`
	}
	_, _, err = PrepareCodexProjectOutbound(c.Request.Context(), account, []byte(`{"input":`+deep+`}`), nil)
	require.Error(t, err)
	c, _, _ = responsePrivacyRequest(t, h, 103, "turn", "")
	_, headerOnly, err := PrepareCodexProjectOutbound(c.Request.Context(), account, []byte(`{"input":"`+projectTestID+`"}`), http.Header{"x-codex-project-id": []string{projectTestID}})
	require.NoError(t, err)
	require.NotContains(t, string(headerOnly), projectTestID)
	_, _, err = PrepareCodexProjectOutbound(context.Background(), account, []byte(`{"input":"project_id: `+projectTestID+`"}`), nil)
	require.Error(t, err)
}

func projectSSE(kind, item, text string, sequence int) string {
	data, _ := json.Marshal(map[string]any{"type": kind, "item_id": item, "output_index": 0, "content_index": 0, "delta": text, "sequence_number": sequence})
	return "event: " + kind + "\ndata: " + string(data) + "\n\n"
}

func TestProjectIdentitySSEEverySplitAndInterleaving(t *testing.T) {
	h, account, _, _ := responsePrivacySetup(t)
	ctx, _, b := projectTestRequest(t, h, account, 101, `{"input":"project_id: `+projectTestID+`"}`)
	for split := 1; split < len(b); split++ {
		t.Run(fmt.Sprint(split), func(t *testing.T) {
			wire := projectSSE("response.output_text.delta", "msg_1", b[:split], 1) + projectSSE("response.output_text.delta", "msg_2", "unrelated", 2) + projectSSE("response.output_text.delta", "msg_1", b[split:]+"!", 3) + `data: {"type":"response.completed","response":{"output":[]}}` + "\n\n"
			response := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}
			require.NoError(t, maskTurnStateResponse(ctx, account, response))
			out, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.NotContains(t, string(out), b)
			joined := ""
			sequences := []int64{}
			for _, line := range strings.Split(string(out), "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				event := gjson.Parse(strings.TrimPrefix(line, "data: "))
				if event.Get("type").String() == "response.output_text.delta" {
					sequences = append(sequences, event.Get("sequence_number").Int())
					if event.Get("item_id").String() == "msg_1" {
						joined += event.Get("delta").String()
					}
				}
			}
			require.Equal(t, projectTestID+"!", joined)
			require.Equal(t, []int64{1, 2, 3}, sequences)
		})
	}
	for _, wireID := range []string{`\u0030` + b[1:], func() string {
		var s strings.Builder
		for _, r := range b {
			fmt.Fprintf(&s, `\u%04x`, r)
		}
		return s.String()
	}()} {
		for split := 1; split < len(wireID); split++ {
			wire := projectSSE("response.output_text.delta", "msg", wireID[:split], 1) + projectSSE("response.output_text.delta", "msg", wireID[split:], 2)
			response := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}
			require.NoError(t, maskTurnStateResponse(ctx, account, response))
			out, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			var joined strings.Builder
			for _, line := range strings.Split(string(out), "\n") {
				if strings.HasPrefix(line, "data: ") {
					joined.WriteString(gjson.Parse(strings.TrimPrefix(line, "data: ")).Get("delta").String())
				}
			}
			var decoded string
			require.NoError(t, json.Unmarshal([]byte(`"`+joined.String()+`"`), &decoded))
			require.Equal(t, projectTestID, decoded, "split=%d", split)
		}
	}
	for _, arguments := range []string{`{"projectId":"` + b + `"}`, `{"projectId":"\u0030` + b[1:] + `","other":"中文"}`} {
		wire := ""
		for i, ch := range []rune(arguments) {
			wire += projectSSE("response.function_call_arguments.delta", "fc_1", string(ch), i)
		}
		wire += `data: {"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":0,"content_index":0}` + "\n\n"
		response := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}
		require.NoError(t, maskTurnStateResponse(ctx, account, response))
		out, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		var joined strings.Builder
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "data: ") {
				joined.WriteString(gjson.Parse(strings.TrimPrefix(line, "data: ")).Get("delta").String())
			}
		}
		require.True(t, gjson.Valid(joined.String()))
		require.Equal(t, projectTestID, gjson.Get(joined.String(), "projectId").String())
	}
}

func TestProjectIdentityFinalHTTPExecutors(t *testing.T) {
	h, account, _, _ := responsePrivacySetup(t)
	previous := GetResinConfig()
	t.Cleanup(func() { SetResinConfig(previous) })
	type projectWire struct {
		body    []byte
		headers http.Header
	}
	sent := make(chan projectWire, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readUpstreamRequestBody(r)
		sent <- projectWire{body, r.Header.Clone()}
		text := gjson.GetBytes(body, "input.0.content.0.text").String()
		w.Header().Set("Content-Type", "application/json")
		payload, _ := json.Marshal(map[string]any{"output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]string{"type": "output_text", "text": text}}}}})
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "project-mapping-test"})
	for _, route := range []string{"native", "relay", "compact", "relay-compact"} {
		t.Run(route, func(t *testing.T) {
			c, body, _ := responsePrivacyRequest(t, h, 101, "turn", "")
			c.Request.Header, body = accountIdentityFixture(t, false, true)
			c.Request = c.Request.WithContext(context.WithValue(WithCodexIdentityStore(c.Request.Context(), h.db), transportOwnerContextKey{}, "project-test-user"))
			c.Request = c.Request.WithContext(ensureTransportTrace(c.Request.Context()))
			body, _ = sjson.SetBytes(body, "client_metadata.project_id", projectTestID)
			body, _ = sjson.SetBytes(body, "client_metadata.projectId", projectTestID)
			body, _ = sjson.SetBytes(body, "client_metadata.workspace_id", projectTestID)
			canonical := diagnosticMetadataObject(gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata")).Raw
			canonical, _ = sjson.Set(canonical, "project_id", projectTestID)
			canonical, _ = sjson.Set(canonical, "workspace_id", projectTestID)
			body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata", canonical)
			c.Request.Header.Set("X-Codex-Project-Id", projectTestID)
			c.Request.Header.Set("X-Codex-Workspace-Id", projectTestID)
			body, _ = sjson.SetBytes(body, "input", []any{map[string]any{"role": "user", "content": []any{map[string]string{"type": "input_text", "text": projectTestID}}}})
			selected := account
			if strings.HasPrefix(route, "relay") {
				selected = &auth.Account{DBID: 42015, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: server.URL, APIKey: "fake-key"}
			}
			selected.CustomHeaders = map[string]string{"X-Codex-Project-Id": "custom-original", "X-Codex-Workspace-Id": "custom-original", "X-Codex-Turn-Metadata": `{"project_id":"custom-original"}`}
			var response *http.Response
			var err error
			switch route {
			case "native":
				response, err = ExecuteRequest(c.Request.Context(), selected, body, "session", "", "key", nil, c.Request.Header, false)
			case "compact":
				response, err = ExecuteCompactRequest(c.Request.Context(), selected, body, "session", "", "key", nil, c.Request.Header)
			case "relay":
				response, err = ExecuteOpenAIResponsesRequest(c.Request.Context(), selected, body, "", c.Request.Header)
			case "relay-compact":
				response, err = ExecuteOpenAIResponsesCompactRequest(c.Request.Context(), selected, body, "", c.Request.Header)
			}
			require.NoError(t, err)
			out, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			captured := <-sent
			wire := captured.body
			require.NotContains(t, string(wire), projectTestID)
			require.NotContains(t, string(wire), "project_mapping")
			require.Contains(t, string(out), projectTestID)
			trace := snapshotUpstreamTrace(c.Request.Context())
			require.NotNil(t, trace.Transport)
			require.NotNil(t, trace.Transport.OutboundIdentity)
			require.NotNil(t, trace.Transport.OutboundIdentity.ProjectMapping)
			d := trace.Transport.OutboundIdentity.ProjectMapping
			require.Equal(t, projectTestID, d.Changes[0].Original)
			require.Positive(t, d.Changes[0].Replaced)
			require.Positive(t, d.Changes[0].Restored)
			_, binding := protocolIdentityBinding(c.Request.Context(), selected)
			alias := gjson.GetBytes(wire, "input.0.content.0.text").String()
			for _, name := range []string{"X-Codex-Project-Id", "X-Codex-Workspace-Id"} {
				require.Equal(t, alias, captured.headers.Get(name), name)
			}
			require.NoError(t, ValidateCodexOutboundMetadata(wire, captured.headers))
			encodedHeaders, _ := json.Marshal(captured.headers)
			require.NotContains(t, string(encodedHeaders), projectTestID)
			require.NotContains(t, string(encodedHeaders), "custom-original")
			require.NotContains(t, string(encodedHeaders), "project_mapping")
			if strings.Contains(route, "compact") {
				require.False(t, gjson.GetBytes(wire, "client_metadata").Exists())
			} else {
				for _, field := range []string{"project_id", "projectId", "workspace_id"} {
					require.Equal(t, alias, gjson.GetBytes(wire, "client_metadata."+field).String(), field)
				}
				require.Equal(t, alias, gjson.Get(gjson.GetBytes(wire, "client_metadata.x-codex-turn-metadata").String(), "project_id").String())
			}
			pair, found, err := h.db.RestoreCodexProjectID(t.Context(), binding, alias)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, database.CodexProtocolPair{Public: projectTestID, Upstream: alias}, pair)
		})
	}
}
