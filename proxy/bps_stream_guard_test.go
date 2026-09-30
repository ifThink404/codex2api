package proxy

import (
	"bufio"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/proxy/plugins"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func shortBPSKeepalive(t *testing.T, interval time.Duration) {
	t.Helper()
	previous := bpsStreamKeepalive
	bpsStreamKeepalive = interval
	t.Cleanup(func() { bpsStreamKeepalive = previous })
}

// readBPSGuardEvent reads one SSE frame from the guard and returns its data.
func readBPSGuardEvent(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	var data string
	for {
		line, err := reader.ReadString('\n')
		require.NoError(t, err)
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			return data
		}
		if value, ok := strings.CutPrefix(line, "data: "); ok {
			data = value
		}
	}
}

func TestBPSStreamGuardPassesFramesThroughUnchanged(t *testing.T) {
	stream := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n" +
		": comment\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\"}}\n\n"
	guard := newBPSStreamGuard(io.NopCloser(strings.NewReader(stream)), nil)
	out, err := io.ReadAll(guard)
	require.NoError(t, err)
	require.Equal(t, stream, string(out))
}

func TestBPSStreamGuardRepeatsInProgressWhileUpstreamIsSilent(t *testing.T) {
	shortBPSKeepalive(t, 20*time.Millisecond)
	upstream, writer := io.Pipe()
	guard := newBPSStreamGuard(upstream, nil)
	defer guard.Close()
	reader := bufio.NewReader(guard)

	go func() {
		_, _ = io.WriteString(writer, "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\",\"output\":[]}}\n\n")
	}()
	require.Equal(t, "response.created", gjson.Get(readBPSGuardEvent(t, reader), "type").String())
	keepalive := gjson.Parse(readBPSGuardEvent(t, reader))
	require.Equal(t, "response.in_progress", keepalive.Get("type").String())
	require.Equal(t, "resp_1", keepalive.Get("response.id").String())
	require.False(t, keepalive.Get("sequence_number").Exists(), "a keepalive must not reuse an upstream sequence number")

	go func() {
		_, _ = io.WriteString(writer, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\"}}\n\n")
		_ = writer.Close()
	}()
	for {
		event := gjson.Parse(readBPSGuardEvent(t, reader))
		if event.Get("type").String() == "response.completed" {
			break
		}
		require.Equal(t, "response.in_progress", event.Get("type").String())
	}
	rest, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Empty(t, rest)
}

func TestBPSStreamGuardWaitsForTheResponseToStart(t *testing.T) {
	shortBPSKeepalive(t, 10*time.Millisecond)
	upstream, writer := io.Pipe()
	guard := newBPSStreamGuard(upstream, nil)
	defer guard.Close()
	go func() {
		time.Sleep(60 * time.Millisecond)
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n")
		_ = writer.Close()
	}()
	out, err := io.ReadAll(guard)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(out), "data: "), "no keepalive before response.created")
}

func TestBPSStreamGuardCloseInterruptsUpstream(t *testing.T) {
	upstream, writer := io.Pipe()
	defer writer.Close()
	guard := newBPSStreamGuard(upstream, nil)
	require.NoError(t, guard.Close())
	_, err := writer.Write([]byte("data: {}\n\n"))
	require.Error(t, err)
}

func TestBPSStreamRedactorLetsKeepaliveOvertakeHeldFrames(t *testing.T) {
	r := newBPSStreamRedactor()
	held, err := r.push("response.output_text.delta", []byte(`{"type":"response.output_text.delta","item_id":"m","output_index":0,"content_index":0,"delta":"Basis Po"}`))
	require.NoError(t, err)
	require.Empty(t, held)
	frames, err := r.push("response.in_progress", []byte(`{"type":"response.in_progress","response":{"id":"resp_1"}}`))
	require.NoError(t, err)
	require.Len(t, frames, 1)
	require.Equal(t, "response.in_progress", frames[0].Event)
	rest, err := r.finish()
	require.NoError(t, err)
	require.Len(t, rest, 1)
	require.Equal(t, "response.output_text.delta", rest[0].Event)
}

const bpsCutoffCreated = "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\",\"model\":\"gpt-5.5\",\"output\":[]}}\n\n"

func bpsCutoffItem(index int, item string) string {
	return "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"sequence_number\":" + strconv.Itoa(2*index+1) + ",\"output_index\":" + strconv.Itoa(index) + ",\"item\":" + item + "}\n\n" +
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"sequence_number\":" + strconv.Itoa(2*index+2) + ",\"output_index\":" + strconv.Itoa(index) + ",\"item\":" + item + "}\n\n"
}

const (
	bpsCutoffReasoning  = `{"type":"reasoning","id":"rs_1","summary":[]}`
	bpsCutoffAnswer     = `{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"done"}]}`
	bpsCutoffCommentary = `{"type":"message","id":"msg_2","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"checking"}]}`
	bpsCutoffToolCall   = `{"type":"function_call","id":"fc_1","call_id":"call_1","name":"shell","arguments":"{}"}`
)

// failingBPSStream ends with a transport error instead of a clean EOF.
type failingBPSStream struct{ io.Reader }

func (s failingBPSStream) Read(p []byte) (int, error) {
	n, err := s.Reader.Read(p)
	if err == io.EOF {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

func TestBPSStreamGuardCompletesCutOffStreams(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream string
		fail   bool
		want   bool
	}{
		{name: "final answer", stream: bpsCutoffCreated + bpsCutoffItem(0, bpsCutoffReasoning) + bpsCutoffItem(1, bpsCutoffAnswer), want: true},
		{name: "tool call", stream: bpsCutoffCreated + bpsCutoffItem(0, bpsCutoffToolCall), want: true},
		{name: "transport error after the answer", stream: bpsCutoffCreated + bpsCutoffItem(0, bpsCutoffAnswer), fail: true, want: true},
		{name: "reasoning only", stream: bpsCutoffCreated + bpsCutoffItem(0, bpsCutoffReasoning)},
		{name: "commentary before tools", stream: bpsCutoffCreated + bpsCutoffItem(0, bpsCutoffCommentary)},
		{name: "item still open", stream: bpsCutoffCreated + bpsCutoffItem(0, bpsCutoffAnswer) +
			"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":1,\"item\":" + bpsCutoffReasoning + "}\n\n"},
		{name: "never started", stream: bpsCutoffItem(0, bpsCutoffAnswer)},
		{name: "upstream completed", stream: bpsCutoffCreated + bpsCutoffItem(0, bpsCutoffAnswer) +
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\"}}\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var source io.Reader = strings.NewReader(tc.stream)
			if tc.fail {
				source = failingBPSStream{source}
			}
			cutoffs := 0
			guard := newBPSStreamGuard(io.NopCloser(source), func() { cutoffs++ })
			out, err := io.ReadAll(guard)
			if tc.fail && !tc.want {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			added := strings.TrimPrefix(string(out), tc.stream)
			require.True(t, strings.HasPrefix(string(out), tc.stream), "upstream frames pass through unchanged")
			if !tc.want {
				require.Empty(t, added)
				require.Zero(t, cutoffs)
				return
			}
			require.Equal(t, 1, cutoffs)
			require.True(t, strings.HasPrefix(added, "event: response.completed\ndata: "))
			completed := gjson.Parse(strings.TrimSpace(strings.TrimPrefix(added, "event: response.completed\ndata: ")))
			require.Equal(t, "response.completed", completed.Get("type").String())
			require.Equal(t, "completed", completed.Get("response.status").String())
			require.Equal(t, "resp_1", completed.Get("response.id").String())
			require.Equal(t, "gpt-5.5", completed.Get("response.model").String())
			require.False(t, completed.Get("response.usage").Exists(), "a rebuilt response carries no usage")
			items := completed.Get("response.output").Array()
			last := items[len(items)-1]
			require.Contains(t, []string{"msg_1", "fc_1"}, last.Get("id").String())
			require.Greater(t, completed.Get("sequence_number").Int(), int64(0))
		})
	}
}

func TestBPSStreamGuardDoesNotCompleteForAClosedClient(t *testing.T) {
	upstream, writer := io.Pipe()
	cutoffs := 0
	guard := newBPSStreamGuard(upstream, func() { cutoffs++ })
	reader := bufio.NewReader(guard)
	go func() { _, _ = io.WriteString(writer, bpsCutoffCreated+bpsCutoffItem(0, bpsCutoffAnswer)) }()
	for range 3 {
		readBPSGuardEvent(t, reader)
	}
	require.NoError(t, guard.Close())
	_ = writer.Close()
	time.Sleep(20 * time.Millisecond)
	require.Zero(t, cutoffs)
}

func TestBPSPluginMarksCutoffCompletionInUsage(t *testing.T) {
	req := plugins.NewRequest("req-cutoff", plugins.KindResponses, nil, nil, 0)
	guard := newBPSStreamGuard(io.NopCloser(strings.NewReader(bpsCutoffCreated+bpsCutoffItem(0, bpsCutoffAnswer))), func() {
		req.SetUsageErrorKind(BPSPluginID, BPSCutoffCompletedKind)
	})
	_, err := io.ReadAll(guard)
	require.NoError(t, err)
	require.Equal(t, BPSCutoffCompletedKind, req.UsageErrorKind(BPSPluginID))
}
