package proxy

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"
)

// BPS stream guard (adopted from upstream's official Excel BPS bridge,
// basispoints/stream.go): upstream frames pass through byte for byte, and
// while the upstream stays silent after the response started, the guard
// repeats response.in_progress. Codex treats a stream idle for five minutes
// as broken and retries the whole turn, which long reasoning can otherwise
// hit. It runs below the response transformer, so every frame it adds is
// projected and redacted like an upstream frame.

// bpsStreamKeepalive is how long the upstream may stay silent before the
// guard repeats response.in_progress. Tests shorten it.
var bpsStreamKeepalive = 15 * time.Second

const bpsStreamFrameLimit = 16 << 20

type bpsStreamFrame struct {
	raw  []byte
	data []byte
}

type bpsStreamGuard struct {
	*io.PipeReader
	upstream io.ReadCloser
	once     sync.Once
	err      error
	closed   atomic.Bool
}

func newBPSStreamGuard(upstream io.ReadCloser) *bpsStreamGuard {
	reader, writer := io.Pipe()
	g := &bpsStreamGuard{PipeReader: reader, upstream: upstream}
	go func() {
		err := g.run(writer)
		_ = g.closeUpstream()
		_ = writer.CloseWithError(err)
	}()
	return g
}

func (g *bpsStreamGuard) closeUpstream() error {
	g.once.Do(func() { g.err = g.upstream.Close() })
	return g.err
}

// Close interrupts a blocked upstream read or pipe write.
func (g *bpsStreamGuard) Close() error {
	g.closed.Store(true)
	return errors.Join(g.PipeReader.Close(), g.closeUpstream())
}

func (g *bpsStreamGuard) run(writer io.Writer) error {
	frames := make(chan bpsStreamFrame)
	readDone := make(chan error, 1)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		readDone <- readBPSStreamFrames(g.upstream, func(frame bpsStreamFrame) bool {
			select {
			case frames <- frame:
				return true
			case <-stop:
				return false
			}
		})
	}()
	lastWrite := time.Now()
	write := func(raw []byte) error {
		if _, err := writer.Write(raw); err != nil {
			return err
		}
		lastWrite = time.Now()
		return nil
	}
	var started []byte
	terminal := false
	interval := bpsStreamKeepalive
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case frame := <-frames:
			if err := write(frame.raw); err != nil {
				return err
			}
			event := gjson.ParseBytes(frame.data)
			switch event.Get("type").String() {
			case "response.created", "response.in_progress":
				if response := event.Get("response"); response.IsObject() {
					started = []byte(response.Raw)
				}
			case "response.completed", "response.done", "response.failed", "response.incomplete", "error":
				terminal = true
			}
		case err := <-readDone:
			return err
		case <-ticker.C:
			if started != nil && !terminal && time.Since(lastWrite) >= interval {
				if err := write(bpsStreamEventFrame("response.in_progress", fmt.Sprintf(`{"type":"response.in_progress","response":%s}`, started))); err != nil {
					return err
				}
			}
		}
	}
}

func bpsStreamEventFrame(event, data string) []byte {
	return []byte("event: " + event + "\ndata: " + data + "\n\n")
}

// readBPSStreamFrames splits an SSE stream into frames ending at a blank
// line; a trailing frame without one is still delivered.
func readBPSStreamFrames(reader io.Reader, consume func(bpsStreamFrame) bool) error {
	br := bufio.NewReaderSize(reader, 64*1024)
	var raw bytes.Buffer
	var data [][]byte
	flush := func() bool {
		if raw.Len() == 0 {
			return true
		}
		frame := bpsStreamFrame{raw: bytes.Clone(raw.Bytes()), data: bytes.Join(data, []byte("\n"))}
		raw.Reset()
		data = nil
		return consume(frame)
	}
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			raw.Write(line)
			if raw.Len() > bpsStreamFrameLimit {
				return errors.New("BPS SSE event exceeds 16 MiB")
			}
			trimmed := bytes.TrimRight(line, "\r\n")
			if len(trimmed) == 0 {
				if !flush() {
					return io.EOF
				}
			} else if value, ok := bytes.CutPrefix(trimmed, []byte("data:")); ok {
				data = append(data, bytes.TrimPrefix(value, []byte(" ")))
			}
		}
		if err != nil {
			if !flush() {
				return io.EOF
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// bpsStreamIsEventStream reports whether a BPS response body is an SSE
// stream the guard should wrap.
func bpsStreamIsEventStream(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "text/event-stream")
}
