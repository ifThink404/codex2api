package admin

import (
	"github.com/codex2api/internal/upstreamprivacy"
	"github.com/gin-gonic/gin"
)

// Covers preflight JSON errors too. SSE payloads also pass through the shared
// event redactor; model deltas are joined/filtered in the upstream reader.
type testResponseWriter struct{ gin.ResponseWriter }

func (w testResponseWriter) Write(data []byte) (int, error) {
	return w.ResponseWriter.Write(upstreamprivacy.Bytes(data))
}
func (w testResponseWriter) WriteString(data string) (int, error) {
	return w.ResponseWriter.WriteString(upstreamprivacy.Text(data))
}
