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
		{"信封值非法时退回顶层 model", "", `{"response":{"model":"bad model"},"model":"gpt-5.4"}`, "response.created", "gpt-5.4"},
		{"两处都非法才放弃", "", `{"response":{"model":"bad model"},"model":"sk-leaked"}`, "response.created", ""},
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

// 只有信封事件才解析。SSE 读循环一次串流几百上千帧，其中 output_item.done 可能
// 带 20KB 的 encrypted_content；非信封帧必须在任何 JSON 扫描之前就被挡掉。
func TestObserveUpstreamResponseModelOnlyParsesEnvelopeEvents(t *testing.T) {
	// 构造一个「带 model 却不该被采信」的非信封帧：真实 delta 不带 model，
	// 这里刻意放一个，闸门没生效就会被观测到。
	delta := []byte(`{"type":"response.output_text.delta","model":"gpt-forged","delta":"hi"}`)
	for _, event := range []string{
		"response.output_text.delta",
		"response.output_item.done",
		"response.reasoning_summary_text.delta",
		"codex.rate_limits",
		"error",
	} {
		if got := observeUpstreamResponseModel("", delta, event); got != "" {
			t.Fatalf("%s 不是信封事件，不该被解析，got %q", event, got)
		}
	}
	// 调用方没给事件名时按载荷里的 type 判定，同样挡掉。
	if got := observeUpstreamResponseModel("", delta, ""); got != "" {
		t.Fatalf("载荷 type 为非信封事件时也必须挡掉，got %q", got)
	}

	created := []byte(`{"type":"response.created","response":{"model":"gpt-5.4-codex"}}`)
	if got := observeUpstreamResponseModel("", created, "response.created"); got != "gpt-5.4-codex" {
		t.Fatalf("response.created 必须被观测，got %q", got)
	}
	completed := []byte(`{"type":"response.completed","response":{"model":"gpt-5.4-thinking"}}`)
	if got := observeUpstreamResponseModel("gpt-5.4-codex", completed, "response.completed"); got != "gpt-5.4-thinking" {
		t.Fatalf("response.completed 必须被观测并覆盖，got %q", got)
	}
	// 非流式整体响应体既没有事件名也没有 type，必须照常解析。
	body := []byte(`{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.4"}`)
	if got := observeUpstreamResponseModel("", body, ""); got != "gpt-5.4" {
		t.Fatalf("非流式响应体必须照常解析，got %q", got)
	}
}

// 空载荷不应把已观测到的值清掉。
func TestObserveUpstreamResponseModelKeepsCurrentOnEmptyPayload(t *testing.T) {
	if got := observeUpstreamResponseModel("gpt-5.4", nil, "response.completed"); got != "gpt-5.4" {
		t.Fatalf("空载荷必须保持原值，got %q", got)
	}
}
