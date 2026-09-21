package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestURLPrivacyMetadataAndBusinessBoundaries(t *testing.T) {
	const original = "https://gateway.example/v1"
	const target = "https://chatgpt.com/backend-api/codex"
	account := &auth.Account{DBID: 42, AccountID: "account-url", AccessToken: "test-token"}
	input := `[{"role":"user","content":"https://gateway.example/v1"},{"type":"function_call","name":"get","call_id":"call_1","arguments":"{\"base_url\":\"https://gateway.example/v1\"}"},{"type":"function_call_output","call_id":"call_1","output":"{\"note\":\"https://gateway.example/v1\"}"}]`
	body := []byte(`{"input":` + input + `,"instructions":"` + original + `","client_metadata":{"baseUrl":"` + original + `","note":"使用` + original + `访问","x-codex-turn-metadata":"{\"openai_base_url\":\"` + original + `\",\"encoded\":\"{\\\"items\\\":[{\\\"copy\\\":\\\"` + original + `/responses\\\"}],\\\"signature\\\":\\\"` + original + `\\\"}\"}"}}`)
	headers := http.Header{"X-Copy": {original + "/responses"}, "Authorization": {"Bearer " + original}, "Host": {"gateway.example"}}
	ctx, out, heads, err := PrepareCodexURLPrivacy(context.Background(), account, body, headers)
	require.NoError(t, err)
	require.Equal(t, input, gjson.GetBytes(out, "input").Raw)
	require.Equal(t, original, gjson.GetBytes(out, "instructions").String())
	require.Equal(t, target, gjson.GetBytes(out, "client_metadata.baseUrl").String())
	require.Equal(t, "使用"+target+"访问", gjson.GetBytes(out, "client_metadata.note").String())
	meta := gjson.Parse(gjson.GetBytes(out, "client_metadata.x-codex-turn-metadata").String())
	require.Equal(t, target, meta.Get("openai_base_url").String())
	encoded := gjson.Parse(meta.Get("encoded").String())
	require.Equal(t, target+"/responses", encoded.Get("items.0.copy").String())
	require.Equal(t, original, encoded.Get("signature").String())
	require.Equal(t, target+"/responses", heads.Get("X-Copy"))
	require.Equal(t, headers.Get("Authorization"), heads.Get("Authorization"))
	require.Equal(t, headers.Get("Host"), heads.Get("Host"))
	require.Equal(t, original+"/responses", headers.Get("X-Copy"), "caller headers mutated")
	_, again, againHeaders, err := PrepareCodexURLPrivacy(ctx, account, out, heads)
	require.NoError(t, err)
	require.Equal(t, out, again)
	require.Equal(t, heads, againHeaders)
	// Existing auxiliary mapping must not hash the replacement URL again.
	out, err = PrepareCodexFunctionalFields(ctx, account, out, heads, "caller")
	require.NoError(t, err)
	require.Equal(t, target, gjson.GetBytes(out, "client_metadata.baseUrl").String())
	// The input integrity guard receives exactly the original business bytes.
	preserved := context.WithValue(ctx, sessionOutboundEpochContextKey{}, &sessionOutboundEpoch{record: database.SessionContinuityRecord{PreserveRestartInput: true}, preservedInput: []byte(input)})
	require.NoError(t, ValidatePreservedSessionInput(preserved, out))
}

func TestURLPrivacyRequestScopedRestore(t *testing.T) {
	account := &auth.Account{DBID: 42, AccountID: "account-url", AccessToken: "test-token"}
	for _, original := range []string{"https://user-a.example/v1", "https://user-b.example/api"} {
		t.Run(original, func(t *testing.T) {
			t.Parallel()
			body, _ := json.Marshal(map[string]any{"client_metadata": map[string]string{"base_url": original}})
			ctx, _, _, err := PrepareCodexURLPrivacy(context.Background(), account, body, nil)
			require.NoError(t, err)
			const official = "https://chatgpt.com/backend-api/codex"
			echo := []byte(`{"metadata":{"base_url":"` + official + `","note":"使用 ` + official + `/responses"},"output":[{"type":"message","content":[{"type":"output_text","text":"` + official + `"}]},{"type":"function_call","arguments":"{\"metadata\":{\"base_url\":\"` + official + `\"}}"}]}`)
			restored, err := maskResponsePayload(ctx, account, echo, true)
			require.NoError(t, err)
			require.Equal(t, original, gjson.GetBytes(restored, "metadata.base_url").String())
			require.Equal(t, "使用 "+original+"/responses", gjson.GetBytes(restored, "metadata.note").String())
			require.JSONEq(t, gjson.GetBytes(echo, "output").Raw, gjson.GetBytes(restored, "output").Raw)
			foreign, err := restoreCodexURLMetadata(ctx, &auth.Account{DBID: 43, AccountID: "another"}, []byte(`{"base_url":"`+official+`"}`))
			require.NoError(t, err)
			require.Equal(t, official, gjson.GetBytes(foreign, "base_url").String())
			stream := "data: {\"type\":\"response.created\",\"response\":" + string(echo) + "}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"" + official + "\"}\n\ndata: [DONE]\n\n"
			resp := &http.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}
			require.NoError(t, maskTurnStateResponse(ctx, account, resp))
			raw, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Contains(t, string(raw), `"base_url":"`+original+`"`)
			require.Contains(t, string(raw), `"delta":"`+official+`"`)
		})
	}
}

func TestURLPrivacyRegistrationHeadersAndAmbiguity(t *testing.T) {
	const first = "https://first.example/v1"
	const second = "https://second.example/v1"
	const target = "https://api.openai.com/v1"
	account := &auth.Account{DBID: 42, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://actual-relay.example/v1", APIKey: "key", CustomHeaders: map[string]string{"X-Base-Url": first, "X-Note": "使用 " + first + "/responses"}}
	ctx, out, _, err := PrepareCodexURLPrivacy(context.Background(), account, []byte(`{"metadata":{"note":"使用 `+first+`/responses","other":"https://ordinary.example"}}`), nil)
	require.NoError(t, err)
	require.Equal(t, "使用 "+target+"/responses", gjson.GetBytes(out, "metadata.note").String())
	require.Equal(t, "https://ordinary.example", gjson.GetBytes(out, "metadata.other").String())
	req, _ := http.NewRequest(http.MethodPost, account.BaseURL+"/responses", nil)
	applyAccountCustomHeaders(req, account)
	req.Header, err = FinalizeCodexURLHeaders(ctx, req.Header)
	require.NoError(t, err)
	require.Equal(t, account.BaseURL+"/responses", req.URL.String())
	require.Equal(t, target, req.Header.Get("X-Base-Url"))
	require.Equal(t, "使用 "+target+"/responses", req.Header.Get("X-Note"))
	require.NoError(t, restoreCodexURLHeaders(ctx, account, req.Header))
	require.Equal(t, first, req.Header.Get("X-Base-Url"))
	require.Equal(t, "使用 "+first+"/responses", req.Header.Get("X-Note"))
	// Unregistered notes are not evidence that an arbitrary website is a gateway.
	_, unregistered, _, err := PrepareCodexURLPrivacy(context.Background(), &auth.Account{}, []byte(`{"client_metadata":{"note":"使用 `+first+`"}}`), nil)
	require.NoError(t, err)
	require.Contains(t, string(unregistered), first)
	ctx, _, _, err = PrepareCodexURLPrivacy(context.Background(), account, []byte(`{"metadata":{"base_url":"`+second+`"}}`), nil)
	require.NoError(t, err)
	restored, err := restoreCodexURLMetadata(ctx, account, []byte(`{"base_url":"`+target+`"}`))
	require.NoError(t, err)
	require.Contains(t, string(restored), target, "ambiguous originals must not be guessed")
}

func TestURLPrivacyBoundariesAndDuplicateKeys(t *testing.T) {
	const base = "https://gateway.example/v1"
	require.Equal(t, "https://gateway.example/v10", replaceCodexURLs(base+"0", map[string]string{base: "https://official.example/v1"}))
	require.Equal(t, "https://gateway.example.evil/v1", replaceCodexURLs("https://gateway.example.evil/v1", map[string]string{"https://gateway.example": "https://official.example"}))
	require.Equal(t, "https://official.example/v1/responses", replaceCodexURLs(base+"/responses", map[string]string{base + "/": "https://official.example/v1/"}))
	require.Equal(t, base+"/responses/compact", replaceCodexURLs(base+"/responses/compact", map[string]string{base: "https://other.example/v1", base + "/responses": ""}))
	require.Equal(t, base+"/responses", replaceCodexURLs(base+"/responses", map[string]string{base: "https://other.example/v1", base + "/responses": base + "/responses"}))
	account := &auth.Account{DBID: 42, AccessToken: "key"}
	untouched := []byte(`{"input":"base_url=https://gateway.example/v1"}`)
	_, unchanged, _, err := PrepareCodexURLPrivacy(context.Background(), account, untouched, nil)
	require.NoError(t, err)
	require.Equal(t, untouched, unchanged)
	_, out, _, err := PrepareCodexURLPrivacy(context.Background(), account, []byte(`{"client_metadata":{"base_url":"`+base+`","x-codex-turn-metadata":{"thread_id":"a","thread_id":"b"}}}`), nil)
	require.NoError(t, err)
	require.Equal(t, 2, strings.Count(string(out), `"thread_id"`))
	require.Error(t, validateCodexMetadataDuplicates(out, false, 0))
}

func TestURLPrivacyLegacyAliasAndMetadataEvents(t *testing.T) {
	h, account, _, _ := responsePrivacySetup(t)
	c, _, _ := responsePrivacyRequest(t, h, 101, "url-legacy", "")
	ctx := WithCodexIdentityStore(c.Request.Context(), h.db)
	const original = "https://old-gateway.example/v1"
	const alias = "meta_0123456789012345678901234567890123456789"
	db, binding := protocolIdentityBinding(ctx, account)
	require.NoError(t, db.PutCodexProtocolPair(ctx, binding, "metadata", database.CodexProtocolPair{Public: original, Upstream: alias}))
	ctx, out, _, err := PrepareCodexURLPrivacy(ctx, account, []byte(`{"client_metadata":{"base_url":"`+alias+`","note":"使用 `+original+`"}}`), nil)
	require.NoError(t, err)
	const official = "https://chatgpt.com/backend-api/codex"
	require.Equal(t, official, gjson.GetBytes(out, "client_metadata.base_url").String())
	require.Equal(t, "使用 "+official, gjson.GetBytes(out, "client_metadata.note").String())
	for _, kind := range []string{"response.metadata", "codex.response.metadata", "responsesapi.response.metadata"} {
		out, err = maskResponsePayload(ctx, account, []byte(`{"type":"`+kind+`","base_url":"`+official+`"}`), false)
		require.NoError(t, err)
		require.Equal(t, original, gjson.GetBytes(out, "base_url").String())
	}
	// Existing responses using the old aliases still restore through their
	// persisted, owner-scoped metadata records.
	out, err = maskResponsePayload(ctx, account, []byte(`{"metadata":{"base_url":"`+alias+`"}}`), true)
	require.NoError(t, err)
	require.Equal(t, original, gjson.GetBytes(out, "metadata.base_url").String())
}

func TestURLPrivacyDiagnosticsAtTraceStart(t *testing.T) {
	request := transportTestContext()
	account := &auth.Account{DBID: 17, AccessToken: "key"}
	metadata := map[string]string{}
	for i := 0; i < 12; i++ {
		metadata[fmt.Sprintf("entry%d", i)] = fmt.Sprintf(`{"base_url":"https://name:secret@gateway%d.example/v1?token=hidden#private"}`, i)
	}
	body, _ := json.Marshal(map[string]any{"client_metadata": metadata})
	ctx, out, _, err := PrepareCodexURLPrivacy(request.Request.Context(), account, body, nil)
	require.NoError(t, err)
	// Trace creation happens after outbound preparation on the HTTP path.
	beginUpstreamTrace(ctx, account, "", false)
	d := snapshotUpstreamTrace(ctx).Transport.OutboundIdentity.URLMapping
	require.NotNil(t, d)
	require.Len(t, d.Changes, 8)
	require.Equal(t, 4, d.Omitted)
	require.True(t, d.AmbiguousRestore)
	require.Greater(t, d.RewriteCount, 0)
	logged, err := json.Marshal(d)
	require.NoError(t, err)
	for _, secret := range []string{"secret", "hidden", "private"} {
		require.NotContains(t, string(logged), secret)
	}
	require.NotContains(t, string(out), "url_mapping")
	require.NotContains(t, string(out), "gateway")
}
