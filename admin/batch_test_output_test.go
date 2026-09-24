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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBatchTestStreamsIncludeActualOutputWithoutAnotherRequest(test *testing.T) {
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
	require.Equal(test, http.StatusOK, response.Code)
	progress := map[int64]batchOperationEvent{}
	for _, line := range strings.Split(response.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event batchOperationEvent
		require.NoError(test, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event))
		if event.Type == "progress" {
			progress[event.AccountID] = event
		}
	}
	require.Len(test, progress, 2)
	for _, account := range accounts {
		event := progress[account.DBID]
		assert.Equal(test, "success", event.Status)
		assert.Equal(test, "测试通过", event.Message)
		assert.Equal(test, http.StatusOK, event.HTTPStatus)
		assert.Equal(test, "你好！", event.Output)
		assert.False(test, event.OutputTruncated)
		assert.Equal(test, "gpt-4o-mini", event.TestModel)
		assert.Equal(test, "gpt-6-sol", event.ResponseModel)
		require.NotNil(test, event.ResponseFieldCount)
		assert.Equal(test, 3, *event.ResponseFieldCount)
	}
	assert.EqualValues(test, 2, upstreamCalls.Load())
}

func TestBatchTestOutputKeepsTerminalSnapshotsAndFailureStatus(test *testing.T) {
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
		test.Run(scenario.name, func(test *testing.T) {
			output := &batchTestOutput{}
			ctx := context.WithValue(context.Background(), batchTestOutputContextKey{}, output)
			stream := "data: " + strings.Join(scenario.events, "\n\ndata: ") + "\n\n"
			response := &http.Response{Body: io.NopCloser(strings.NewReader(stream))}
			status, _ := readRecycleBinTestStream(ctx, response)
			assert.Equal(test, scenario.status, status)
			assert.Equal(test, scenario.output, string(output.text))
		})
	}
}

func TestBatchTestResponseShapeExcludesGatewayBillingMetadata(test *testing.T) {
	output := &batchTestOutput{}
	output.observeResponses([]byte(`{"object":"response","model":"gpt-6-astra","output":[],"codex2api_billing":{"service_tier":"priority"}}`))
	require.NotNil(test, output.responseFieldCount)
	require.Equal(test, 3, *output.responseFieldCount)
	require.Equal(test, "gpt-6-astra", output.responseModel)
}

func TestBatchTestOutputIsBoundedAndAccountScoped(test *testing.T) {
	first, second := &batchTestOutput{}, &batchTestOutput{}
	first.append(strings.Repeat("中", batchTestOutputLimit))
	first.append("more")
	second.append("独立结果")
	assert.LessOrEqual(test, len(first.text), batchTestOutputLimit)
	assert.True(test, utf8.Valid(first.text))
	assert.True(test, first.truncated)
	assert.Equal(test, "独立结果", string(second.text))
	assert.False(test, second.truncated)
	exact := &batchTestOutput{}
	exact.append(strings.Repeat("a", batchTestOutputLimit))
	assert.False(test, exact.truncated)
	exact.append("b")
	assert.True(test, exact.truncated)
}

func TestBatchTestOutputSupportsClaudeTextCallback(test *testing.T) {
	output := &batchTestOutput{}
	ctx := context.WithValue(context.Background(), batchTestOutputContextKey{}, output)
	response := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   io.NopCloser(strings.NewReader("data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Claude reply\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")),
	}
	status, _ := readClaudeMessagesStream(ctx, response, batchTestOutputFromContext(ctx).append)
	assert.Equal(test, "success", status)
	assert.Equal(test, "Claude reply", string(output.text))
}
