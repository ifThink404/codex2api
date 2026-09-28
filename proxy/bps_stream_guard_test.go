package proxy

import (
	"bufio"
	"io"
	"strings"
	"testing"
	"time"

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
	guard := newBPSStreamGuard(io.NopCloser(strings.NewReader(stream)))
	out, err := io.ReadAll(guard)
	require.NoError(t, err)
	require.Equal(t, stream, string(out))
}

func TestBPSStreamGuardRepeatsInProgressWhileUpstreamIsSilent(t *testing.T) {
	shortBPSKeepalive(t, 20*time.Millisecond)
	upstream, writer := io.Pipe()
	guard := newBPSStreamGuard(upstream)
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
	guard := newBPSStreamGuard(upstream)
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
	guard := newBPSStreamGuard(upstream)
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
