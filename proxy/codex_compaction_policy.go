package proxy

import (
	"net/http"
	"strings"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

// ValidateNativeCompactionPolicy checks current control metadata, never user
// messages or tool output. A historical compaction item alone is not a new
// compaction request. The caller exempts requests actually routed to BPS.
// Dedicated /responses/compact executors already speak a
// native compaction protocol and do not pass through this ordinary-turn guard.
func ValidateNativeCompactionPolicy(account *auth.Account, body []byte, headers http.Header) error {
	if !account.NativeCompactionOnlyEnabled() || requestBodyHasCompactionTrigger(body) {
		return nil
	}
	flat := gjson.GetBytes(body, "client_metadata")
	metadata := diagnosticMetadataObject(flat.Get("x-codex-turn-metadata"))
	if !metadata.IsObject() {
		// Current body metadata takes priority over an older compatibility header.
		if flat.Get("request_kind").Exists() || flat.Get("compaction").IsObject() {
			metadata = flat
		} else {
			metadata = diagnosticMetadataObject(gjson.Parse(headers.Get(codexTurnMetadataHeader)))
		}
	}
	if !strings.EqualFold(strings.TrimSpace(metadata.Get("request_kind").String()), "compaction") && !metadata.Get("compaction").IsObject() {
		return nil
	}
	return &Error{
		Code: "native_compaction_required", Type: ErrorTypeInvalidRequest,
		HTTPStatus: http.StatusBadRequest,
		Message:    "此账号仅允许原生远程压缩，请升级或调整 Codex 客户端，同时将 [model_providers.custom] 下的 name 改成 name = \"OpenAI\"。",
	}
}
