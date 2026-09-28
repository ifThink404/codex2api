package proxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// BPS stream guard (adopted from upstream's official Excel BPS bridge,
// basispoints/stream.go): upstream frames pass through byte for byte, and
// while the upstream stays silent after the response started, the guard
// repeats response.in_progress. Codex treats a stream idle for five minutes
// as broken and retries the whole turn, which long reasoning can otherwise
// hit. When the upstream closes after finishing every item it started and the
// last one ends a turn, the guard completes the response itself (marked
// basispoints_cutoff_completed in the usage log, since it carries no usage).
// It runs below the response transformer, so every frame it adds is projected
// and redacted like an upstream frame.

// bpsStreamKeepalive is how long the upstream may stay silent before the
// guard repeats response.in_progress. Tests shorten it.
var bpsStreamKeepalive = 15 * time.Second

const bpsStreamFrameLimit = 16 << 20

type bpsStreamFrame struct {
	raw  []byte
	data []byte
}

// BPSCutoffCompletedKind marks usage rows whose response.completed the
// guard rebuilt after the upstream closed early.
const BPSCutoffCompletedKind = "basispoints_cutoff_completed"

type bpsStreamGuard struct {
	*io.PipeReader
	upstream io.ReadCloser
	once     sync.Once
	err      error
	closed   atomic.Bool
	// onCutoff runs when the guard completes a cut-off stream.
	onCutoff func()
}

type bpsFinishedItem struct {
	index int
	raw   string
}

func newBPSStreamGuard(upstream io.ReadCloser, onCutoff func()) *bpsStreamGuard {
	reader, writer := io.Pipe()
	g := &bpsStreamGuard{PipeReader: reader, upstream: upstream, onCutoff: onCutoff}
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
	// started, itemsAdded and finished describe the response so far, for
	// keepalive frames and for completing a stream cut off after its last item.
	var started []byte
	terminal := false
	itemsAdded := 0
	var finished []bpsFinishedItem
	sequence := int64(-1)
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
			if number := event.Get("sequence_number"); number.Type == gjson.Number {
				sequence = max(sequence, number.Int())
			}
			switch event.Get("type").String() {
			case "response.created", "response.in_progress":
				if response := event.Get("response"); response.IsObject() {
					started = []byte(response.Raw)
				}
			case "response.output_item.added":
				itemsAdded++
			case "response.output_item.done":
				if item := event.Get("item"); item.IsObject() {
					index := len(finished)
					if number := event.Get("output_index"); number.Type == gjson.Number {
						index = int(number.Int())
					}
					finished = append(finished, bpsFinishedItem{index: index, raw: item.Raw})
				}
			case "response.completed", "response.done", "response.failed", "response.incomplete", "error":
				terminal = true
			}
		case err := <-readDone:
			if terminal || g.closed.Load() || bpsStreamClosedLocally(err) {
				// A client that went away has nobody to complete the response for.
				return err
			}
			completed := bpsCutoffCompletion(started, itemsAdded, finished, sequence)
			if completed == nil {
				return err
			}
			reason := "closed"
			if err != nil {
				reason = err.Error()
			}
			log.Printf("[bps] upstream stream ended before response.completed (%s); completing it from %d finished items", reason, len(finished))
			if werr := write(bpsStreamEventFrame("response.completed", string(completed))); werr != nil {
				return werr
			}
			if g.onCutoff != nil {
				g.onCutoff()
			}
			return nil
		case <-ticker.C:
			if started != nil && !terminal && time.Since(lastWrite) >= interval {
				if err := write(bpsStreamEventFrame("response.in_progress", fmt.Sprintf(`{"type":"response.in_progress","response":%s}`, started))); err != nil {
					return err
				}
			}
		}
	}
}

// bpsCutoffCompletion rebuilds response.completed when the upstream closed
// after finishing every item it started and the last one ends a turn: a final
// answer message or a client tool call. Anything else (reasoning, commentary
// before tool calls) means the stream was cut mid-turn, which is left to the
// client's retry instead of being reported as a finished answer.
func bpsCutoffCompletion(started []byte, itemsAdded int, finished []bpsFinishedItem, sequence int64) []byte {
	if started == nil || len(finished) == 0 || len(finished) < itemsAdded {
		return nil
	}
	sort.SliceStable(finished, func(i, j int) bool { return finished[i].index < finished[j].index })
	last := gjson.Parse(finished[len(finished)-1].raw)
	kind := last.Get("type").String()
	endsTurn := kind == "function_call" || kind == "custom_tool_call" || kind == "local_shell_call" ||
		kind == "message" && last.Get("phase").String() != "commentary"
	if !endsTurn {
		return nil
	}
	output := make([]string, 0, len(finished))
	for _, item := range finished {
		output = append(output, item.raw)
	}
	response, err := sjson.SetBytes(bytes.Clone(started), "status", "completed")
	if err == nil {
		response, err = sjson.SetRawBytes(response, "output", []byte("["+strings.Join(output, ",")+"]"))
	}
	if err != nil {
		return nil
	}
	payload := []byte(`{"type":"response.completed"}`)
	if sequence >= 0 {
		payload, _ = sjson.SetBytes(payload, "sequence_number", sequence+1)
	}
	payload, err = sjson.SetRawBytes(payload, "response", response)
	if err != nil {
		return nil
	}
	return payload
}

// bpsStreamClosedLocally reports read errors caused by this side closing the
// stream (client disconnect or cancellation), not the upstream dropping it.
func bpsStreamClosedLocally(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, http.ErrBodyReadAfterClose) || errors.Is(err, io.ErrClosedPipe)
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
