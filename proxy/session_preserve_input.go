package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const preservedInputSnapshotKey = "session_preserved_input_snapshot"

type preservedInputPreparedKey struct{}
type preservedInputReplayKey struct{}

type preservedInputReplay struct {
	previousID string
	original   []byte
	expanded   []byte
}

// Capture only the owner-scoped cache expansion, before payload rules and
// identity rewriting. Keep raw client items (including opaque history and large
// integers); the translator's other edits are not implicitly authorized here.
func capturePreservedInputReplay(request *gin.Context, prepared responsesBodyPreparation) {
	var replay *preservedInputReplay
	if raw, ok := request.Get(preservedInputSnapshotKey); ok {
		if body, ok := raw.([]byte); ok && prepared.PreviousResponseID != "" &&
			gjson.GetBytes(body, "previous_response_id").String() == prepared.PreviousResponseID &&
			prepared.CacheLookup.Kind == responseCacheLookupHit && !prepared.Bypassed {
			input := gjson.GetBytes(body, "input")
			validInput := !input.Exists() || input.Type == gjson.Null || input.IsArray() || input.Type == gjson.String
			items := append([]json.RawMessage(nil), prepared.CacheLookup.Items...)
			switch {
			case input.IsArray():
				for _, item := range input.Array() {
					items = append(items, json.RawMessage(item.Raw))
				}
			case input.Type == gjson.String:
				items = append(items, json.RawMessage(`{"role":"user","content":`+input.Raw+`}`))
			}
			if expanded, err := json.Marshal(items); err == nil && validInput {
				replay = &preservedInputReplay{prepared.PreviousResponseID, []byte(input.Raw), expanded}
			}
		}
	}
	request.Request = request.Request.WithContext(context.WithValue(request.Request.Context(), preservedInputReplayKey{}, replay))
}

// Restore protected input once, retaining an authenticated cache expansion.
// Final outbound input consistency enforcement is temporarily disabled.
func PreparePreservedInputTransport(ctx context.Context, body []byte) (context.Context, []byte, error) {
	if !PreserveSessionInput(ctx) || ctx.Value(preservedInputPreparedKey{}) != nil {
		return ctx, body, nil
	}
	epoch := outboundEpochFromContext(ctx)
	if epoch == nil {
		return ctx, body, nil
	}
	if replay, _ := ctx.Value(preservedInputReplayKey{}).(*preservedInputReplay); replay != nil {
		if !bytes.Equal(replay.original, epoch.preservedInput) {
			return ctx, nil, preserveInputError("续写输入快照发生变化，请恢复完整历史后重试。")
		}
		if epoch.handler == nil {
			return ctx, nil, preserveInputError("无法核实续写历史归属，请恢复完整历史后重试。")
		}
		known, cancel := epoch.restartContextVerifier(ctx)
		allowed := known("previous_response_id", replay.previousID)
		cancel()
		if !allowed {
			return ctx, nil, preserveInputError("续写历史不属于当前账号或会话代次，请恢复完整历史后重试。")
		}
		copyEpoch := *epoch
		copyEpoch.preservedInput = replay.expanded
		epoch = &copyEpoch
		ctx = context.WithValue(ctx, sessionOutboundEpochContextKey{}, epoch)
	}
	if len(epoch.preservedInput) == 0 {
		return ctx, body, nil
	}
	updated, err := sjson.SetRawBytes(body, "input", epoch.preservedInput)
	if err != nil {
		return ctx, nil, preserveInputError("无法恢复完整输入，请恢复完整历史后重试。")
	}
	return context.WithValue(ctx, preservedInputPreparedKey{}, true), updated, nil
}

func ValidateSessionOutboundRequest(ctx context.Context, account *auth.Account, body []byte) error {
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		return codexAccountIdentityError("出站请求正文无效，已停止发送。")
	}
	if err := ValidateBackgroundAccountMatch(ctx, account); err != nil {
		return err
	}
	// TEMPORARY: allow history identity rewrites while the preserve-input
	// conflict is investigated. Restore ValidatePreservedSessionInput afterward.
	return nil
}

// PreserveSessionInput follows the committed segment, not a live setting that
// could silently downgrade an existing conversation after a reload.
func PreserveSessionInput(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	if fork := forkAccountFallbackFromContext(ctx); fork != nil && fork.PreserveInput {
		return true
	}
	if plan, _ := ctx.Value(sessionAccountFailoverContextKey{}).(*sessionAccountFailoverPlan); plan != nil && plan.PreserveInput {
		return true
	}
	epoch := outboundEpochFromContext(ctx)
	return epoch != nil && epoch.record.PreserveRestartInput
}

func preserveInputError(message string) *Error {
	return &Error{Code: "codex_session_failover_context_required", Type: ErrorTypeInvalidRequest, HTTPStatus: http.StatusBadRequest, Message: message}
}

func preserveSessionRestartInput(payload map[string]json.RawMessage, headers http.Header, original []byte, report *database.SessionContextCleanup) ([]byte, http.Header, *database.SessionContextCleanup, error) {
	input := gjson.ParseBytes(payload["input"])
	_, hasConversation := conversationReferenceID(gjson.ParseBytes(payload["conversation"]))
	// cleanSessionRestartContext has already authenticated these references.
	hasContinuation := gjson.ParseBytes(payload["previous_response_id"]).String() != "" || hasConversation
	if !hasContinuation && !(input.Type == gjson.String && strings.TrimSpace(input.String()) != "" || input.IsArray() && len(input.Array()) > 0) {
		return nil, headers, report, preserveInputError("完整保留 input 模式需要客户端提供完整输入数组或文本，不能仅依赖旧账号的续写状态。")
	}
	if input.IsArray() && !hasContinuation {
		pairing, missingOutput := inspectPreservedToolPairing(input)
		if pairing != nil {
			report.ToolPairing = pairing
			report.ToolsBefore = summarizeSessionTools(original)
			if missingOutput {
				return nil, headers, report, preserveInputError("完整保留 input 模式发现工具结果缺少对应调用，请恢复完整历史后重试；不会删除工具结果重试。")
			}
			return nil, headers, report, preserveInputError("完整保留 input 模式缺少动态工具搜索结果对应的调用，请恢复完整历史后重试。")
		}
	}
	// RawMessage avoids converting tool schema integers through float64.
	cleaned, err := json.Marshal(payload)
	if err == nil {
		err = checkSessionToolPreservation(original, cleaned, report)
	}
	return cleaned, headers, report, err
}

// ValidatePreservedSessionInput catches any later input rewrite before send.
// The snapshot is request-local; no prompt or ciphertext is persisted here.
func ValidatePreservedSessionInput(ctx context.Context, body []byte) error {
	if !PreserveSessionInput(ctx) {
		return nil
	}
	epoch := outboundEpochFromContext(ctx)
	if epoch == nil || len(epoch.preservedInput) == 0 {
		return nil
	}
	actual := gjson.GetBytes(body, "input").Raw
	if actual == string(epoch.preservedInput) {
		return nil
	}
	decode := func(raw []byte) (any, error) {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		err := decoder.Decode(&value)
		return value, err
	}
	want, first := decode(epoch.preservedInput)
	got, second := decode([]byte(actual))
	if first == nil && second == nil && reflect.DeepEqual(want, got) {
		return nil
	}
	return preserveInputError("完整保留 input 模式检测到出站输入被其他处理修改，已停止发送；请检查输入改写规则或恢复完整上下文。")
}
