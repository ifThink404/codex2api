package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/upstreamprivacy"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestBPSProbeModeDoesNotChangeAccount(t *testing.T) {
	for _, saved := range []bool{false, true} {
		a := &auth.Account{DBID: 7, CodexBPS: saved}
		for _, mode := range []string{"auto", "codex", "bps"} {
			ctx, err := WithCodexTestMode(t.Context(), mode)
			require.NoError(t, err)
			use, err := codexRequestUsesBPS(ctx, a)
			require.NoError(t, err)
			require.Equal(t, mode == "bps" || mode == "auto" && saved, use)
			require.Equal(t, saved, a.CodexBPS)
		}
	}
	ctx, err := WithCodexTestMode(t.Context(), "bps")
	require.NoError(t, err)
	require.Error(t, ValidateCodexTestMode(ctx, &auth.Account{UpstreamType: auth.UpstreamOpenAIResponses}))
	_, err = WithCodexTestMode(t.Context(), "invalid")
	require.Error(t, err)
}

func TestBPSProbeOverrideReachesChosenEndpoint(t *testing.T) {
	t.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "probe.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	a := &auth.Account{DBID: 1695, AccountID: accountIdentitySampleAccount, AccessToken: "test-access"}
	var endpoint string
	installClaudeBoundaryTransport(t, a, func(r *http.Request) (*http.Response, error) {
		endpoint = r.URL.String()
		return &http.Response{StatusCode: 200, Request: r, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"resp_probe","output":[]}`))}, nil
	})
	for _, mode := range []string{"bps", "codex"} {
		a.CodexBPS = mode != "bps" // deliberately opposite to the chosen route
		ctx, err := WithCodexTestMode(WithCodexAccountTestIdentityStore(t.Context(), db, a), mode)
		require.NoError(t, err)
		ctx = WithCodexAccountTestRawResponse(ctx, a)
		headers, body := accountIdentityFixture(t, false, true)
		resp, err := ExecuteRequest(ctx, a, body, "probe", "", "test-key", nil, headers, false)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		if mode == "bps" {
			require.Equal(t, CodexBPSBaseURL+"/responses", endpoint)
			require.Len(t, resp.Header.Get(codexTurnStateHeader), 292)
		} else {
			require.Equal(t, CodexBaseURL+"/responses", endpoint)
			require.Empty(t, resp.Header.Get(codexTurnStateHeader))
		}
		require.Equal(t, mode != "bps", a.CodexBPS)
	}
}

func TestBPSSynthetic292LifecycleAndNoOutboundRestoration(t *testing.T) {
	h, a, _, _ := responsePrivacySetup(t)
	issue := func(turn string) string {
		c, _, _ := responsePrivacyRequest(t, h, 101, turn, "")
		d := &CodexBPSDiagnostic{Mode: "bps"}
		req, _ := http.NewRequestWithContext(context.WithValue(c.Request.Context(), codexBPSDiagnosticKey{}, d), "POST", CodexBPSBaseURL+"/responses", nil)
		resp := &http.Response{StatusCode: 200, Request: req, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"output\":[]}}\n\n"))}
		var err error
		finishTurnStateResponse(c.Request.Context(), a, &resp, &err)
		require.NoError(t, err)
		alias := resp.Header.Get(codexTurnStateHeader)
		require.Len(t, alias, 292)
		raw, err := base64.URLEncoding.Strict().DecodeString(alias)
		require.NoError(t, err)
		require.Len(t, raw, 217)
		require.Equal(t, byte(0x80), raw[0])
		require.InDelta(t, time.Now().Unix(), binary.BigEndian.Uint64(raw[1:9]), 3)
		record, found, err := h.db.ReadCodexTurnState(t.Context(), alias)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, database.CodexTurnStateSyntheticBPS, record.Kind)
		all, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Contains(t, string(all), alias)
		require.Zero(t, d.UpstreamTurnState.Length)
		require.Equal(t, 292, d.ClientTurnState.Length)
		return alias
	}
	first := issue("turn-1")
	require.Equal(t, first, issue("turn-1"))
	require.NotEqual(t, first, issue("turn-2"))
	c, body, identity := responsePrivacyRequest(t, h, 101, "turn-1", "")
	c.Request.Header.Set(codexTurnStateHeader, first)
	body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-state", first)
	body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.x-codex-turn-state", first)
	h.bindTurnStateSession(c, body, identity)
	body = normalizeTurnStateIngress(c, body)
	require.NotContains(t, string(body), first)
	require.Empty(t, c.Request.Header.Get(codexTurnStateHeader))
	out, headers := PrepareCodexTurnStateOutbound(c.Request.Context(), a, []byte(fmt.Sprintf(`{"client_metadata":{"x-codex-turn-state":%q}}`, first)), http.Header{codexTurnStateHeader: []string{first}})
	require.NotContains(t, string(out), first)
	require.Empty(t, headers.Get(codexTurnStateHeader))
	require.NotContains(t, string(out), "local-bps-turn-state")
}

func TestBPSPublicStreamRedactsAcrossEverySplit(t *testing.T) {
	for _, kind := range []string{"response.output_text.delta", "response.function_call_arguments.delta", "response.custom_tool_call_input.delta"} {
		for _, value := range []string{"bps.openai.com", `b\u0070s.openai.com`, "bps%2eopenai.com", "https://bps.openai.com/basispoints/api/responses", "https://hidden.invalid/basispoints/api/responses/compact"} {
			for split := 1; split < len(value); split++ {
				var frames bytes.Buffer
				for _, part := range []string{value[:split], value[split:]} {
					data, _ := json.Marshal(map[string]any{"type": kind, "item_id": "item-1", "delta": part})
					fmt.Fprintf(&frames, "data: %s\n\n", data)
				}
				frames.WriteString("data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n")
				r := &turnStateStream{ctx: t.Context(), raw: true, body: io.NopCloser(strings.NewReader(frames.String())), reader: bufio.NewReader(strings.NewReader(frames.String()))}
				all, err := io.ReadAll(r)
				require.NoError(t, err)
				var combined string
				for _, line := range strings.Split(string(all), "\n") {
					if strings.HasPrefix(line, "data: ") {
						combined += gjson.Get(strings.TrimPrefix(line, "data: "), "delta").String()
					}
				}
				require.NotEqual(t, value, combined, "split %d type %s", split, kind)
				require.Equal(t, upstreamprivacy.Text(value), combined)
				require.NotContains(t, strings.ToLower(string(all)), "bps.openai.com")
			}
		}
	}
}

func TestBPSSyntheticLegacyClientsRequireEchoForReuse(t *testing.T) {
	h, a, other, _ := responsePrivacySetup(t)
	newSession := func() *turnStateSession {
		return &turnStateSession{handler: h, scope: "legacy-user-root", rootKey: "legacy-root", incoming: make(map[string]database.CodexTurnStateRecord), issued: make(map[string]database.CodexTurnStateRecord)}
	}
	s := newSession()
	source := s.syntheticSource(t.Context(), a)
	alias, err := s.issueKind(t.Context(), a, source, "response_header", database.CodexTurnStateSyntheticBPS)
	require.NoError(t, err)
	require.Equal(t, source, s.syntheticSource(t.Context(), a))
	record, found, err := h.db.ReadCodexTurnState(t.Context(), alias)
	require.NoError(t, err)
	require.True(t, found)
	next := newSession()
	require.NotEqual(t, source, next.syntheticSource(t.Context(), a))
	next.incoming[alias] = record
	require.Equal(t, source, next.syntheticSource(t.Context(), a))
	require.NotEqual(t, source, next.syntheticSource(t.Context(), other))
}
