package proxy

import "testing"

// 覆盖上游自报模型的取值与拒收规则：这个值只进日志展示，不参与计费/调度，
// 但它直接来自上游响应，必须先当成不可信输入过一遍白名单。
func TestObserveUpstreamResponseModel(t *testing.T) {
	long := ""
	for i := 0; i < 101; i++ {
		long += "a"
	}
	cases := []struct {
		name    string
		current string
		payload string
		event   string
		want    string
	}{
		{"response.model 优先", "", `{"type":"response.created","response":{"model":"gpt-5.4-codex"}}`, "response.created", "gpt-5.4-codex"},
		{"顶层 model 兜底", "", `{"model":"gpt-5.4"}`, "", "gpt-5.4"},
		{"response.model 空时退回顶层 model", "", `{"response":{"model":""},"model":"gpt-5.4"}`, "", "gpt-5.4"},
		{"非终态不覆盖已有值", "gpt-5.4", `{"type":"response.in_progress","response":{"model":"gpt-9"}}`, "response.in_progress", "gpt-5.4"},
		{"终态覆盖已有值", "gpt-5.4", `{"type":"response.completed","response":{"model":"gpt-5.4-codex"}}`, "response.completed", "gpt-5.4-codex"},
		{"response.failed 也是终态", "gpt-5.4", `{"response":{"model":"gpt-5.4-thinking"}}`, "response.failed", "gpt-5.4-thinking"},
		{"response.incomplete 也是终态", "gpt-5.4", `{"response":{"model":"a-b"}}`, "response.incomplete", "a-b"},
		{"response.done 也是终态", "gpt-5.4", `{"response":{"model":"a-c"}}`, "response.done", "a-c"},
		{"response.cancelled 也是终态", "gpt-5.4", `{"response":{"model":"a-d"}}`, "response.cancelled", "a-d"},
		{"response.canceled 也是终态", "gpt-5.4", `{"response":{"model":"a-e"}}`, "response.canceled", "a-e"},
		{"字符集非法整条丢弃", "", `{"model":"gpt-5.4 codex"}`, "", ""},
		{"控制字符丢弃", "", `{"model":"gpt\n5"}`, "", ""},
		{"超过 100 字符丢弃", "", `{"model":"` + long + `"}`, "", ""},
		{"sk- 前缀丢弃", "", `{"model":"sk-proj-abcdef"}`, "", ""},
		{"eyJ 前缀丢弃", "", `{"model":"eyJhbGciOiJIUzI1NiJ9"}`, "", ""},
		{"非法 JSON 丢弃", "", `{"model":"gpt-5.4"`, "", ""},
		{"缺字段保持原值", "gpt-5.4", `{"type":"response.output_text.delta","delta":"hi"}`, "response.output_text.delta", "gpt-5.4"},
		{"非字符串类型丢弃", "", `{"model":123}`, "", ""},
		{"两侧空白会被裁掉", "", `{"model":"  gpt-5.4  "}`, "", "gpt-5.4"},
		{"终态但值非法时保持原值", "gpt-5.4", `{"response":{"model":"bad model"}}`, "response.completed", "gpt-5.4"},
		{"允许的符号集", "", `{"model":"anthropic/claude-opus-5:1m_v1.2"}`, "", "anthropic/claude-opus-5:1m_v1.2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := observeUpstreamResponseModel(tc.current, []byte(tc.payload), tc.event); got != tc.want {
				t.Fatalf("observeUpstreamResponseModel(%q, %s, %q) = %q, want %q", tc.current, tc.payload, tc.event, got, tc.want)
			}
		})
	}
}

// 事件名缺省时用载荷里的 type 判定终态：SSE 的 event: 行不是必填，
// 只按参数判定会让 response.completed 无法覆盖早期的 response.created。
func TestObserveUpstreamResponseModelFallsBackToPayloadType(t *testing.T) {
	got := observeUpstreamResponseModel("gpt-5.4", []byte(`{"type":"response.completed","response":{"model":"gpt-5.4-codex"}}`), "")
	if got != "gpt-5.4-codex" {
		t.Fatalf("载荷 type 为终态时必须覆盖，got %q", got)
	}
}

// 空载荷不应把已观测到的值清掉。
func TestObserveUpstreamResponseModelKeepsCurrentOnEmptyPayload(t *testing.T) {
	if got := observeUpstreamResponseModel("gpt-5.4", nil, "response.completed"); got != "gpt-5.4" {
		t.Fatalf("空载荷必须保持原值，got %q", got)
	}
}
