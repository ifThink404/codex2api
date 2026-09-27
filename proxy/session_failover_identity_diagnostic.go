package proxy

import (
	"context"
	"errors"

	"github.com/codex2api/database"
)

// Only report fields produced locally, never err.Error(), Cause, or identifiers.
func sessionFailoverIdentityFailure(fingerprint *CodexFingerprint, err error) *database.SessionFailoverIdentityFailure {
	detail := &database.SessionFailoverIdentityFailure{Stage: "identity_claim", Status: "failed", Code: "identity_claim_failed"}
	if fingerprint != nil && fingerprint.accountIdentityDiagnostic != nil {
		diagnostic := fingerprint.accountIdentityDiagnostic
		detail.Status = diagnostic.Status
		if diagnostic.FailureStage != "" {
			detail.Stage = diagnostic.FailureStage
		}
	}
	if err == nil {
		detail.Stage, detail.Code = "account_mapping", "mapping_missing"
		return detail
	}
	var typed *Error
	if errors.As(err, &typed) {
		switch typed.Code {
		case "codex_session_identity_unavailable", "codex_session_identity_invalid", "codex_session_identity_conflict", "codex_background_account_mismatch":
			detail.Code = typed.Code
		}
		if typed.HTTPStatus >= 400 && typed.HTTPStatus <= 599 {
			detail.HTTPStatus = typed.HTTPStatus
		}
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		detail.Code = "identity_deadline_exceeded"
	case errors.Is(err, context.Canceled):
		detail.Code = "identity_canceled"
	}
	return detail
}
