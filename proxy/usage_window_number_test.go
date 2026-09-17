package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// Codex 客户端的窗口号取自 <uuid>:<n> 形式的 window-id。记录的是客户端原始值，
// 指纹收敛改写的是出站副本，与这里无关。
func TestParseCodexWindowNumber(t *testing.T) {
	const uuid = "0199a2b0-1f3c-7c11-8f2e-4b6d9a1c2e30"
	cases := []struct {
		name   string
		header string
		body   string
		want   string
	}{
		{"请求头形式", uuid + ":3", "", "3"},
		{"请求体形式", "", `{"client_metadata":{"x-codex-window-id":"` + uuid + `:12"}}`, "12"},
		{"请求体优先于请求头", uuid + ":3", `{"client_metadata":{"x-codex-window-id":"` + uuid + `:12"}}`, "12"},
		{"都没有", "", "", ""},
		{"空请求体", "", `{}`, ""},
		{"缺冒号", uuid, "", ""},
		{"窗口号非数字", uuid + ":abc", "", ""},
		{"窗口号为负", uuid + ":-1", "", ""},
		{"冒号后为空", uuid + ":", "", ""},
		{"冒号前为空", ":3", "", ""},
		{"超出 uint64 的巨大数字", uuid + ":184467440737095516160", "", ""},
		{"前导零归一", uuid + ":007", "", "7"},
		{"零号窗口", uuid + ":0", "", "0"},
		{"多个冒号取最后一段", "a:b:" + uuid + ":9", "", "9"},
		{"两侧空白", "  " + uuid + ":4  ", "", "4"},
		{"非法 JSON 请求体", "", `{"client_metadata":`, ""},
		{"请求体值非字符串", uuid + ":5", `{"client_metadata":{"x-codex-window-id":7}}`, "5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			header := http.Header{}
			if tc.header != "" {
				header.Set(codexWindowIDHeader, tc.header)
			}
			if got := parseCodexWindowNumber(header, []byte(tc.body)); got != tc.want {
				t.Fatalf("parseCodexWindowNumber(%q, %q) = %q, want %q", tc.header, tc.body, got, tc.want)
			}
		})
	}
}

// nil 请求头 / nil 请求体是 WebSocket 帧与内部请求的常态，不能 panic。
func TestParseCodexWindowNumberHandlesNilInputs(t *testing.T) {
	if got := parseCodexWindowNumber(nil, nil); got != "" {
		t.Fatalf("parseCodexWindowNumber(nil, nil) = %q, want \"\"", got)
	}
}

func TestPopulateUsageWindowNumberFromHTTPRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set(codexWindowIDHeader, "0199a2b0-1f3c-7c11-8f2e-4b6d9a1c2e30:6")

	input := &database.UsageLogInput{}
	populateUsageWindowNumber(c, input)
	if input.WindowNumber != "6" {
		t.Fatalf("WindowNumber = %q, want \"6\"", input.WindowNumber)
	}
}

// WebSocket 的窗口号是逐帧的：握手请求头属于整条连接，落到每一帧上就是串号。
func TestPopulateUsageWindowNumberIgnoresWebSocketHandshakeHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses/ws", nil)
	c.Request.Header.Set("Connection", "Upgrade")
	c.Request.Header.Set("Upgrade", "websocket")
	c.Request.Header.Set(codexWindowIDHeader, "0199a2b0-1f3c-7c11-8f2e-4b6d9a1c2e30:6")

	input := &database.UsageLogInput{}
	populateUsageWindowNumber(c, input)
	if input.WindowNumber != "" {
		t.Fatalf("握手请求头不能落到帧上，WindowNumber = %q", input.WindowNumber)
	}

	// 帧体里的窗口号才算数。
	setRawRequestBody(c, []byte(`{"client_metadata":{"x-codex-window-id":"0199a2b0-1f3c-7c11-8f2e-4b6d9a1c2e30:2"}}`))
	input = &database.UsageLogInput{}
	populateUsageWindowNumber(c, input)
	if input.WindowNumber != "2" {
		t.Fatalf("帧体窗口号 = %q, want \"2\"", input.WindowNumber)
	}
}

// 已经由调用方填好的值不被覆盖。
func TestPopulateUsageWindowNumberKeepsExistingValue(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set(codexWindowIDHeader, "0199a2b0-1f3c-7c11-8f2e-4b6d9a1c2e30:6")

	input := &database.UsageLogInput{WindowNumber: "1"}
	populateUsageWindowNumber(c, input)
	if input.WindowNumber != "1" {
		t.Fatalf("WindowNumber = %q, want \"1\"", input.WindowNumber)
	}
}
