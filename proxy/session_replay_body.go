package proxy

import (
	"strings"

	"github.com/codex2api/api"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Only normalize the copy inspected for migration. Keep ingress identity and
// metadata intact, and leave the actual endpoint's translation path in charge
// of the upstream payload. Chat/Messages histories are valid replay context.
func sessionReplayBody(c *gin.Context, body []byte) ([]byte, *api.APIError) {
	if gjson.GetBytes(body, "input").Exists() || !gjson.GetBytes(body, "messages").IsArray() {
		return body, nil
	}
	var translated []byte
	var err error
	switch {
	case strings.HasSuffix(c.Request.URL.Path, "/chat/completions"):
		translated, err = TranslateRequest(body)
	case strings.HasSuffix(c.Request.URL.Path, "/messages"):
		translated, _, err = TranslateAnthropicToCodex(body, "")
	default:
		return body, nil
	}
	if err == nil {
		body, err = sjson.SetRawBytes(body, "input", []byte(gjson.GetBytes(translated, "input").Raw))
	}
	if err != nil {
		return nil, api.NewAPIError(api.ErrCodeInvalidParameter, "换号请求上下文转换失败，请检查消息和工具格式。", api.ErrorTypeInvalidRequest)
	}
	return body, nil
}
