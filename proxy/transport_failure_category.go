package proxy

import (
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

func transportFailureCodeCategory(code string) string {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "identity_verification_required", "verification_required":
		return "identity_verification_required"
	case "account_banned", "account_deactivated", "account_disabled", "account_suspended":
		return "account_disabled"
	case "access_token_expired", "token_expired", "refresh_token_invalidated":
		return "credential_expired"
	case "authentication_error", "invalid_api_key", "unauthorized", "token_invalid", "token_revoked", "token_invalidated":
		return "authentication"
	case "usage_limit_reached", "workspace_member_usage_limit_reached", "workspace_owner_usage_limit_reached":
		return "usage_limit_exhausted"
	case "quota_exceeded", "quota_exhausted", "insufficient_quota", "billing_limit", "workspace_member_credits_depleted", "workspace_owner_credits_depleted":
		return "quota_exhausted"
	case "rate_limit_error", "rate_limit_exceeded", "rate_limit_reached", "slow_down":
		return "rate_limited"
	case "model_not_supported", "model_not_available", "model_not_found":
		return "model_unsupported"
	case "permission_denied", "organization_disabled", "workspace_deactivated", "deactivated_workspace":
		return "permission_denied"
	case "invalid_request", "invalid_request_error", "invalid_argument", "invalid_prompt", "previous_response_not_found", "invalid_encrypted_content":
		return "invalid_request"
	case "server_error", "server_is_overloaded", "service_unavailable_error", "websocket_connection_limit_reached":
		return "unavailable"
	}
	return ""
}

func classifyTransportDiagnostic(diagnostic *UpstreamTransportDiagnostic) {
	diagnostic.FailureCategory, diagnostic.FailureEvidence = "", ""
	if diagnostic.MessageTooBigSource != "" {
		diagnostic.FailureCategory, diagnostic.FailureEvidence = "message_too_big", diagnostic.MessageTooBigSource
		return
	}
	if diagnostic.ErrorSource == "transport" {
		diagnostic.FailureCategory, diagnostic.FailureEvidence = "transport", "transport_error"
		return
	}
	if !strings.HasPrefix(diagnostic.ErrorSource, "upstream_") {
		return
	}
	for _, candidate := range []struct{ value, evidence string }{
		{diagnostic.IdentityErrorCode, "identity_error_header"},
		{diagnostic.AuthorizationError, "authorization_error_header"},
		{diagnostic.ErrorCode, "error_code"},
		{diagnostic.ErrorType, "error_type"},
	} {
		if diagnostic.ErrorSource == "upstream_ws" && strings.HasSuffix(candidate.evidence, "header") {
			continue
		}
		if category := transportFailureCodeCategory(candidate.value); category != "" {
			// BPS wraps request validation failures in a generic server_error.
			// Preserve explicit identity/quota codes above, but do not interpret
			// this HTTP 422 wrapper as evidence that the account is unavailable.
			if diagnostic.BPS != nil && diagnostic.ErrorSource == "upstream_http" && diagnostic.HTTPStatus == http.StatusUnprocessableEntity && candidate.evidence == "error_type" && strings.EqualFold(strings.TrimSpace(candidate.value), "server_error") {
				diagnostic.FailureCategory, diagnostic.FailureEvidence = "invalid_request", "http_status"
				return
			}
			diagnostic.FailureCategory, diagnostic.FailureEvidence = category, candidate.evidence
			return
		}
	}
	status := diagnostic.HTTPStatus
	if diagnostic.ErrorStage == "ws_handshake" {
		status = diagnostic.HandshakeStatus
	}
	if diagnostic.EventStatus >= 400 {
		status = diagnostic.EventStatus
	}
	switch {
	case status == http.StatusUnauthorized:
		diagnostic.FailureCategory = "authentication"
	case status == http.StatusForbidden:
		diagnostic.FailureCategory = "permission_denied"
	case status == http.StatusTooManyRequests:
		diagnostic.FailureCategory = "rate_limited"
	case status >= 500:
		diagnostic.FailureCategory = "unavailable"
	case status >= 400:
		diagnostic.FailureCategory = "invalid_request"
	default:
		return
	}
	diagnostic.FailureEvidence = "http_status"
	if diagnostic.EventStatus >= 400 {
		diagnostic.FailureEvidence = "event_status"
	}
}

func (observer *TransportObserver) HTTPErrorBody(payload []byte) {
	observer.update(func(diagnostic *UpstreamTransportDiagnostic) {
		if diagnostic.HTTPStatus < 400 && diagnostic.HandshakeStatus < 400 {
			return
		}
		object := gjson.GetBytes(payload, "error")
		if !object.IsObject() {
			object = gjson.GetBytes(payload, "response.error")
		}
		if !object.IsObject() {
			object = gjson.ParseBytes(payload)
		}
		diagnostic.ErrorCode = safeDiagnosticToken(object.Get("code").String())
		diagnostic.ErrorType = safeDiagnosticToken(object.Get("type").String())
	})
}
