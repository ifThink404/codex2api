package proxy

import "context"

type codexIdentityClaimDiagnostic struct {
	Result            string `json:"result"`
	FailureStage      string `json:"failure_stage,omitempty"`
	Reason            string `json:"reason,omitempty"`
	Relaxed           bool   `json:"relaxed"`
	AccountID         int64  `json:"account_id,omitempty"`
	OwnerSource       string `json:"owner_source,omitempty"`
	OwnerHash         string `json:"owner_hash,omitempty"`
	IdentityHash      string `json:"identity_hash,omitempty"`
	ExistingOwnerHash string `json:"existing_owner_hash,omitempty"`
}

func recordCodexIdentityClaim(ctx context.Context, diagnostic *codexIdentityClaimDiagnostic) {
	if epoch := outboundEpochFromContext(ctx); epoch != nil && epoch.preview {
		return
	}
	if audit := upstreamTraceFromContext(ctx); audit != nil {
		audit.mu.Lock()
		defer audit.mu.Unlock()
		audit.identityClaim = diagnostic
	}
}
