package proxy

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestBPSExecutorNormalAndCompact(t *testing.T) {
	t.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "bps.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	a := &auth.Account{DBID: 1695, AccountID: accountIdentitySampleAccount, AccessToken: "test-access", CodexBPS: true, CodexInstallationID: "account-device", CustomHeaders: map[string]string{"X-Openai-Account-Id": "bad-custom-account", "X-Openai-Internal-Basispoints-Client-Device-Id": "bad-device", "Session-Id": "bad-custom-session"}}
	var sentBody []byte
	var sentHeaders http.Header
	var endpoint string
	installClaudeBoundaryTransport(t, a, func(r *http.Request) (*http.Response, error) {
		sentBody, err = io.ReadAll(r.Body)
		require.NoError(t, err)
		sentHeaders, endpoint = r.Header.Clone(), r.URL.String()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"response-test","model":"gpt-5.6-luna","output":[]}`)), Request: r}, nil
	})
	var session, task, turn string
	for _, compact := range []bool{false, true} {
		headers, body := accountIdentityFixture(t, false, true)
		imageData := bpsTestPNG(t)
		body, _ = sjson.SetBytes(body, "model", "codex-auto-review")
		body, _ = sjson.SetBytes(body, "instructions", "Keep the original instructions.")
		body, _ = sjson.SetRawBytes(body, "tools", []byte(`[{"type":"function","name":"echo","parameters":{"type":"object","properties":{"n":{"type":"integer","maximum":9007199254740993}}}}]`))
		body, _ = sjson.SetRawBytes(body, "input", []byte(`[{"type":"additional_tools","tools":[{"type":"custom","name":"note"}]},{"type":"function_call","id":"fc_history","call_id":"call_history","name":"echo","arguments":"{\"n\":1}"},{"type":"function_call_output","call_id":"call_history","output":"1"},{"type":"reasoning","encrypted_content":"opaque-history"}]`))
		body, _ = sjson.SetBytes(body, "input.2.output", []map[string]string{{"type": "input_text", "text": "1"}, {"type": "input_image", "image_url": "data:application/octet-stream;base64," + imageData, "detail": "original"}})
		for _, field := range []string{"service_tier", "include", "text", "tool_choice", "parallel_tool_calls"} {
			body, _ = sjson.SetBytes(body, field, "unsupported-fixture")
		}
		body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-state", "old-native-state")
		headers.Set("X-Codex-Turn-State", "old-native-state")
		c := transportTestContext()
		c.Request = c.Request.WithContext(WithCodexIdentityStore(c.Request.Context(), db))
		var resp *http.Response
		if compact {
			resp, err = ExecuteCompactRequest(c.Request.Context(), a, body, "cache", "", "test-key", nil, headers)
		} else {
			// Force WS in the caller. The BPS route must still use HTTP/SSE.
			resp, err = ExecuteRequest(c.Request.Context(), a, body, "cache", "", "test-key", nil, headers, true)
		}
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.True(t, strings.HasPrefix(endpoint, CodexBPSBaseURL+"/responses"))
		require.Equal(t, "gpt-5.6-luna", gjson.GetBytes(sentBody, "model").String())
		require.Equal(t, "codex-auto-review", gjson.GetBytes(body, "model").String())
		require.Equal(t, "Bearer test-access", sentHeaders.Get("Authorization"))
		require.Equal(t, a.AccountID, sentHeaders.Get("Chatgpt-Account-Id"))
		require.Equal(t, a.AccountID, sentHeaders.Get("X-Openai-Account-Id"))
		require.Equal(t, "account-device", sentHeaders.Get("X-Openai-Internal-Basispoints-Client-Device-Id"))
		require.Empty(t, sentHeaders.Get(codexTurnStateHeader))
		require.Empty(t, sentHeaders.Get(codexTurnMetadataHeader))
		for _, raw := range []string{accountIdentitySampleRoot, "bad-custom", "bad-device", "old-native-state", "9dcfc09b-4e8b-4e25-9052-fefb87224807"} {
			require.NotContains(t, string(sentBody), raw)
		}
		require.Contains(t, string(sentBody), "9007199254740993")
		require.Equal(t, bpsCallerRuntimeInstructions, gjson.GetBytes(sentBody, "input.0.content.0.text").String())
		require.Equal(t, "Keep the original instructions.", gjson.GetBytes(sentBody, "input.2.content.0.text").String())
		require.Equal(t, "note", gjson.GetBytes(sentBody, "input.3.tools.0.name").String())
		require.Equal(t, "call_history", gjson.GetBytes(sentBody, "input.4.call_id").String())
		require.Equal(t, "call_history", gjson.GetBytes(sentBody, "input.5.call_id").String())
		require.Equal(t, "data:image/png;base64,"+imageData, gjson.GetBytes(sentBody, "input.5.output.1.image_url").String())
		require.Equal(t, "original", gjson.GetBytes(sentBody, "input.5.output.1.detail").String())
		require.Equal(t, "opaque-history", gjson.GetBytes(sentBody, "input.6.encrypted_content").String())
		if session == "" {
			session = sentHeaders.Get("Session-Id")
			task = gjson.GetBytes(sentBody, "metadata.task_id").String()
			turn = gjson.GetBytes(sentBody, "metadata.turn_id").String()
		}
		require.Equal(t, session, sentHeaders.Get("Session-Id"))
		require.Equal(t, task, gjson.GetBytes(sentBody, "metadata.task_id").String())
		require.Equal(t, turn, gjson.GetBytes(sentBody, "metadata.turn_id").String())
		if compact {
			require.Equal(t, CodexBPSBaseURL+"/responses/compact", endpoint)
			require.Len(t, gjson.ParseBytes(sentBody).Map(), 3)
		} else {
			require.Equal(t, session, gjson.GetBytes(sentBody, "prompt_cache_key").String())
			require.True(t, gjson.GetBytes(sentBody, "stream").Bool())
		}
		d := snapshotUpstreamTrace(c.Request.Context()).Transport
		require.NotNil(t, d)
		require.Equal(t, "http", d.Transport)
		if compact {
			require.Equal(t, "not_applicable", d.OutboundIdentity.SessionConsistency)
		} else {
			require.Equal(t, "matched", d.OutboundIdentity.SessionConsistency)
		}
		require.Equal(t, "codex-auto-review", d.BPS.RequestedModel)
		require.Equal(t, 1, d.BPS.Images.MIMENormalized)
		require.Equal(t, "gpt-5.6-luna", d.BPS.SentModel)
		require.Contains(t, d.BPS.AdaptedFields, "model: codex-auto-review → gpt-5.6-luna")
		require.Contains(t, d.BPS.RemovedFields, "tool_choice")
		require.NotContains(t, string(sentBody), "bps_compat")
	}
}

func TestBPSRoutePinnedToDurableRootAndPassiveRequests(t *testing.T) {
	h := newWindowAuthorizationHandler(t)
	a := &auth.Account{DBID: 1695, AccessToken: "test", CodexBPS: true, Status: auth.StatusReady}
	h.store.AddAccount(a)
	key := "root::api-key:101"
	h.store.BindSessionAffinity(key, a, "")
	c, body := continuityTestRequest(0, "turn")
	require.Nil(t, h.prepareSessionContinuity(c, requestSessionIdentity{stableIdentity: true}, key, body))
	require.Nil(t, h.commitSessionContinuity(c, a))
	record, found, err := h.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "bps", record.UpstreamMode)
	a.CodexBPS = false
	use, err := codexRequestUsesBPS(c.Request.Context(), a)
	require.NoError(t, err)
	require.True(t, use)
	h.continuityRecords = nil // Restart/cache eviction: child resolves the same persisted parent.
	ctx := context.WithValue(t.Context(), protocolIdentityKey{}, &responseIdentitySession{handler: h, root: key, rootKey: hashRiskIdentity(key)})
	use, err = codexRequestUsesBPS(ctx, a)
	require.NoError(t, err)
	require.True(t, use)
	_, err = h.db.CommitSessionContinuity(t.Context(), "old-root", database.SessionContinuityRecord{AccountID: a.ID(), ThreadID: continuityTestThread})
	require.NoError(t, err)
	a.CodexBPS = true
	ctx = context.WithValue(t.Context(), protocolIdentityKey{}, &responseIdentitySession{handler: h, root: "old", rootKey: "old-root"})
	use, err = codexRequestUsesBPS(ctx, a)
	require.NoError(t, err)
	require.False(t, use)
	use, err = codexRequestUsesBPS(t.Context(), a)
	require.NoError(t, err)
	require.True(t, use) // fresh probe
}

func TestBPSMetadataPrivacyPreservesToolBusinessData(t *testing.T) {
	h, a, _, _ := responsePrivacySetup(t)
	c, _, _ := responsePrivacyRequest(t, h, 101, "turn", "")
	raw := []byte(`{"type":"response.completed","response":{"id":"resp_bps","metadata":{"task_id":"task_secret","turn_id":"turn_secret","nested":{"task_id":"task_secret"}},"output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"echo","arguments":"{\"task_id\":\"business-value\"}"}]}}`)
	out, err := maskResponsePayload(c.Request.Context(), a, raw, false)
	require.NoError(t, err)
	require.NotContains(t, string(out), "task_secret")
	require.NotContains(t, string(out), "turn_secret")
	require.Equal(t, `{"task_id":"business-value"}`, gjson.GetBytes(out, "response.output.0.arguments").String())
	require.Equal(t, "call_1", gjson.GetBytes(out, "response.output.0.call_id").String())
}

func TestBPSUnboundContinuityRestartPinsMode(t *testing.T) {
	h := newWindowAuthorizationHandler(t)
	a := &auth.Account{DBID: 1695, AccountID: accountIdentitySampleAccount, AccessToken: "test", CodexBPS: true, Status: auth.StatusReady}
	h.store.AddAccount(a)
	c, _ := continuityTestRequest(24, "turn")
	state := &sessionContinuityRequest{Key: hashRiskIdentity("restart-bps"), ThreadID: continuityTestThread, Number: 24, Known: true, RestartReason: "unbound_nonzero", Diagnostic: &sessionContinuityDiagnostic{}}
	require.Nil(t, h.commitContinuityRestart(c, a, state))
	require.Equal(t, "bps", state.Record.UpstreamMode)
	a.CodexBPS = false
	use, err := codexRequestUsesBPS(c.Request.Context(), a)
	require.NoError(t, err)
	require.True(t, use)
	record, found, err := h.db.ReadSessionContinuity(t.Context(), state.Key)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "bps", record.UpstreamMode)
}
