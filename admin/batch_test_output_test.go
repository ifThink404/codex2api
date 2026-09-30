package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

func TestBatchTestStreamsIncludeActualOutputWithoutAnotherRequest(t *testing.T) {
	var upstreamCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upstreamCalls.Add(1)
		writer.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"你好！\"}\n\n")
		fmt.Fprint(writer, "data: {\"type\":\"response.output_text.done\",\"text\":\"你好！\"}\n\n")
		fmt.Fprint(writer, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"model\":\"gpt-6-sol\",\"output_text\":\"你好！\"}}\n\n")
	}))
	defer server.Close()
	store := auth.NewStore(nil, nil, nil)
	accounts := []*auth.Account{
		{DBID: 11, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: server.URL, APIKey: "test-key-one", Models: []string{"gpt-4o-mini"}, Status: auth.StatusReady},
		{DBID: 12, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: server.URL, APIKey: "test-key-two", Models: []string{"gpt-4o-mini"}, Status: auth.StatusReady},
	}
	for _, account := range accounts {
		store.AddAccount(account)
	}
	handler := &Handler{store: store}
	response := httptest.NewRecorder()
	requestContext, _ := gin.CreateTestContext(response)
	requestContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/batch-test?stream=true", nil)
	handler.streamBatchTest(requestContext, accounts, 0, handler.runSingleBatchTest)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	progress := map[int64]batchOperationEvent{}
	for _, line := range strings.Split(response.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event batchOperationEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatalf("decode event %q: %v", line, err)
		}
		if event.Type == "progress" {
			progress[event.AccountID] = event
		}
	}
	if len(progress) != 2 {
		t.Fatalf("progress events = %d, want 2: %s", len(progress), response.Body.String())
	}
	for _, account := range accounts {
		event := progress[account.DBID]
		if event.Status != "success" || event.Message != "测试通过" || event.HTTPStatus != http.StatusOK {
			t.Fatalf("account %d event = %+v, want success", account.DBID, event)
		}
		if event.Output != "你好！" || event.OutputTruncated {
			t.Fatalf("account %d output = %q truncated=%v", account.DBID, event.Output, event.OutputTruncated)
		}
		if event.TestModel != "gpt-4o-mini" || event.ResponseModel != "gpt-6-sol" {
			t.Fatalf("account %d models = test %q response %q", account.DBID, event.TestModel, event.ResponseModel)
		}
		if event.ResponseFieldCount == nil || *event.ResponseFieldCount != 3 {
			t.Fatalf("account %d response field count = %v, want 3", account.DBID, event.ResponseFieldCount)
		}
	}
	if calls := upstreamCalls.Load(); calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
}

func TestBatchTestOutputKeepsTerminalSnapshotsAndFailureStatus(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		events []string
		output string
		status string
	}{
		{name: "done only", events: []string{`{"type":"response.output_text.done","text":"done"}`, `{"type":"response.completed"}`}, output: "done", status: "success"},
		{name: "content part", events: []string{`{"type":"response.content_part.done","part":{"text":"part"}}`, `{"type":"response.completed"}`}, output: "part", status: "success"},
		{name: "output item", events: []string{`{"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":"item"}]}}`, `{"type":"response.completed"}`}, output: "item", status: "success"},
		{name: "complete snapshot", events: []string{`{"type":"response.output_text.delta","delta":"first"}`, `{"type":"response.completed","response":{"output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"not-display-output"}]},{"type":"message","content":[{"type":"output_text","text":"first"},{"type":"output_text","text":" second"}]}]}}`}, output: "first second", status: "success"},
		{name: "partial failure", events: []string{`{"type":"response.output_text.delta","delta":"partial"}`, `{"type":"response.failed","response":{"error":{"message":"failed after partial output"}}}`}, output: "partial", status: "failed"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			output := &batchTestOutput{}
			ctx := context.WithValue(context.Background(), batchTestOutputContextKey{}, output)
			stream := "data: " + strings.Join(scenario.events, "\n\ndata: ") + "\n\n"
			response := &http.Response{Body: io.NopCloser(strings.NewReader(stream))}
			status, _ := readRecycleBinTestStream(ctx, response)
			if status != scenario.status || string(output.text) != scenario.output {
				t.Fatalf("status/output = %q/%q, want %q/%q", status, output.text, scenario.status, scenario.output)
			}
		})
	}
}

func TestBatchTestResponseShapeExcludesGatewayBillingMetadata(t *testing.T) {
	output := &batchTestOutput{}
	output.observeResponses([]byte(`{"object":"response","model":"gpt-6-astra","output":[],"codex2api_billing":{"service_tier":"priority"}}`))
	if output.responseFieldCount == nil || *output.responseFieldCount != 3 {
		t.Fatalf("response field count = %v, want 3", output.responseFieldCount)
	}
	if output.responseModel != "gpt-6-astra" {
		t.Fatalf("response model = %q", output.responseModel)
	}
}

func TestBatchTestOutputIsBoundedAndAccountScoped(t *testing.T) {
	first, second := &batchTestOutput{}, &batchTestOutput{}
	first.append(strings.Repeat("中", batchTestOutputLimit))
	first.append("more")
	second.append("独立结果")
	if len(first.text) > batchTestOutputLimit || !utf8.Valid(first.text) || !first.truncated {
		t.Fatalf("first output len=%d valid=%v truncated=%v", len(first.text), utf8.Valid(first.text), first.truncated)
	}
	if string(second.text) != "独立结果" || second.truncated {
		t.Fatalf("second output = %q truncated=%v", second.text, second.truncated)
	}
	exact := &batchTestOutput{}
	exact.append(strings.Repeat("a", batchTestOutputLimit))
	if exact.truncated {
		t.Fatal("output at exactly the limit must not be truncated")
	}
	exact.append("b")
	if !exact.truncated {
		t.Fatal("output past the limit must be truncated")
	}
}

func TestBatchTestOutputSupportsClaudeTextCallback(t *testing.T) {
	output := &batchTestOutput{}
	ctx := context.WithValue(context.Background(), batchTestOutputContextKey{}, output)
	response := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   io.NopCloser(strings.NewReader("data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Claude reply\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")),
	}
	status, _ := readClaudeMessagesStream(ctx, response, batchTestOutputFromContext(ctx).append)
	if status != "success" || string(output.text) != "Claude reply" {
		t.Fatalf("status/output = %q/%q", status, output.text)
	}
}
