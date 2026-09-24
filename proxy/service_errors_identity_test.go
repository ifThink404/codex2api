package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func legacyParentIdentityError(test *testing.T, handler *Handler, request *gin.Context) error {
	test.Helper()
	test.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	account := &auth.Account{DBID: 1695, AccountID: accountIdentitySampleAccount}
	headers, body := accountIdentityFixture(test, false, true)
	parent := "01a08303-49f4-7b53-b545-920f29610317"
	body, err := sjson.SetBytes(body, "client_metadata.parent_thread_id", parent)
	require.NoError(test, err)
	owner := codexIdentityDigest("codex-owner-v1", "credential:test-key")
	rootKey := codexIdentityDigest("codex-account-root-v1", owner, accountIdentitySampleAccount, accountIdentitySampleRoot)
	_, err = handler.db.ResolveCodexIdentityMapping(context.Background(), rootKey, nil, false)
	require.NoError(test, err)
	parentKey := codexIdentityDigest("codex-account-root-v1", owner, accountIdentitySampleAccount, parent)
	_, err = handler.db.ResolveCodexIdentityMapping(context.Background(), parentKey, nil, false)
	require.NoError(test, err)
	request.Request = request.Request.WithContext(WithCodexIdentityStore(request.Request.Context(), handler.db))
	beginUpstreamTrace(request.Request.Context(), account, "", false)
	fingerprint := NewCodexTransportFingerprint(account, headers, body, "cache")
	err = fingerprint.ClaimSessionIdentity(request.Request.Context(), account, "test-key")
	require.ErrorContains(test, err, "父会话尚无可确认的账号级出站映射")
	return err
}

func TestServiceErrorsRecordIdentityRejectionAfterAccountSelection(test *testing.T) {
	handler := newServiceErrorTestHandler(test)
	router := gin.New()
	router.Use(handler.ServiceErrorMiddleware())
	router.POST("/v1/responses", func(request *gin.Context) {
		failure := legacyParentIdentityError(test, handler, request)
		ErrorToGinResponse(request, failure)
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
	require.Equal(test, http.StatusBadRequest, recorder.Code)
	require.Equal(test, "codex_session_identity_unavailable", gjson.Get(recorder.Body.String(), "error.code").String())
	page := serviceErrorTestPage(test, handler)
	require.Len(test, page.Items, 1)
	event := page.Items[0]
	require.Equal(test, http.StatusBadRequest, event.StatusCode)
	require.Equal(test, "codex_session_identity_unavailable", event.Code)
	require.Equal(test, "gateway", gjson.GetBytes(event.UpstreamInfo, "error_source").String())
	require.Equal(test, "identity_validation", gjson.GetBytes(event.UpstreamInfo, "error_stage").String())
	require.Equal(test, int64(1695), gjson.GetBytes(event.UpstreamInfo, "account_id").Int())
	require.Equal(test, "before_payload", gjson.GetBytes(event.UpstreamInfo, "send_phase").String())
	require.Equal(test, "preserve", gjson.GetBytes(event.UpstreamInfo, "outbound_identity.account_mapping.references.0.policy").String())
	require.Equal(test, "01a08303-49f4-7b53-b545-920f29610317", gjson.GetBytes(event.UpstreamInfo, "outbound_identity.account_mapping.references.0.original").String())
}

func TestServiceErrorsRecordClaimConflictBeforeUpstreamStarts(test *testing.T) {
	test.Setenv("CODEX_OUTBOUND_SESSION_MODE", "preserve")
	handler := newServiceErrorTestHandler(test)
	account := &auth.Account{DBID: 7, AccountID: accountIdentitySampleAccount}
	headers, body := accountIdentityFixture(test, false, true)
	first := codexOwnerTestContext(test, handler, "old-owner", true)
	fingerprint := NewCodexTransportFingerprint(account, headers, body, "cache")
	require.NoError(test, fingerprint.ClaimSessionIdentity(first.Request.Context(), account, "private-key"))
	router := gin.New()
	router.Use(handler.ServiceErrorMiddleware())
	router.POST("/v1/responses", func(request *gin.Context) {
		bindTransportOwner(request, verifiedNewAPIPolicyContext{APIKeyID: 1, Platform: "platform", MetaVerified: true, Identity: newAPIIdentity{UserID: "new-owner"}}, true)
		handler.bindCodexIdentityClaims(request)
		fresh := NewCodexTransportFingerprint(account, headers, body, "cache")
		failure := fresh.ClaimSessionIdentity(request.Request.Context(), account, "private-key")
		require.Error(test, failure)
		ErrorToGinResponse(request, failure)
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
	require.Equal(test, http.StatusBadRequest, recorder.Code)
	page := serviceErrorTestPage(test, handler)
	require.Len(test, page.Items, 1)
	upstream := page.Items[0].UpstreamInfo
	require.Equal(test, "not_started", gjson.GetBytes(upstream, "transport").String())
	require.Equal(test, "conflict", gjson.GetBytes(upstream, "identity_claim.result").String())
	require.Equal(test, "signed_newapi", gjson.GetBytes(upstream, "identity_claim.owner_source").String())
	require.NotEmpty(test, gjson.GetBytes(upstream, "identity_claim.existing_owner_hash").String())
	require.NotContains(test, string(upstream), "private-key")
	require.NotContains(test, recorder.Body.String(), "owner_hash")
}

func TestCodexIdentityErrorAPIStatusRemainsClientError(test *testing.T) {
	for _, code := range []string{"codex_session_identity_invalid", "codex_session_identity_conflict", "codex_session_identity_unavailable", "codex_background_account_mismatch"} {
		test.Run(code, func(test *testing.T) {
			require.Equal(test, http.StatusBadRequest, api.HTTPStatusCode(api.ErrorCode(code)))
		})
	}
}

func TestServiceErrorsRecordCommittedIdentityRejectionWithoutLosingCode(test *testing.T) {
	for _, scenario := range []struct {
		name      string
		endpoint  string
		protocol  continuousRetryHTTPProtocol
		errorPath string
	}{
		{"responses", "/v1/responses", continuousRetryProtocolResponses, "response.error"},
		{"chat", "/v1/chat/completions", continuousRetryProtocolChat, "error"},
		{"messages", "/v1/messages", continuousRetryProtocolAnthropic, "error"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			handler := newServiceErrorTestHandler(test)
			router := gin.New()
			router.Use(handler.ServiceErrorMiddleware())
			router.POST(scenario.endpoint, func(request *gin.Context) {
				failure := legacyParentIdentityError(test, handler, request)
				request.Header("Content-Type", "text/event-stream")
				_, err := request.Writer.WriteString(": keepalive\n\n")
				require.NoError(test, err)
				request.Writer.Flush()
				require.True(test, sendCodexIdentityRequestError(request, failure, scenario.protocol))
			})
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, scenario.endpoint, nil))
			require.Equal(test, http.StatusOK, recorder.Code)
			_, payload, found := strings.Cut(recorder.Body.String(), "data: ")
			require.True(test, found)
			require.True(test, gjson.Valid(strings.TrimSpace(payload)))
			require.Equal(test, "codex_session_identity_unavailable", gjson.Get(payload, scenario.errorPath+".code").String())
			require.Equal(test, "invalid_request_error", gjson.Get(payload, scenario.errorPath+".type").String())
			page := serviceErrorTestPage(test, handler)
			require.Len(test, page.Items, 1)
			require.Equal(test, http.StatusBadRequest, page.Items[0].StatusCode)
			require.Equal(test, "sse", page.Items[0].Transport)
			require.Equal(test, "codex_session_identity_unavailable", page.Items[0].Code)
		})
	}
}
