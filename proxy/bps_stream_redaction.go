package proxy

import (
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/codex2api/internal/upstreamprivacy"
	"github.com/codex2api/proxy/plugins"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Cross-frame redaction of BPS source text (ported from fj-server's
// projectStreamBuffer, without its project-identity restoration). Provider
// names and the private BPS host can be split across text deltas, so deltas
// of one channel are joined, redacted and repartitioned into the original
// frames. Frames are held back only while a protected prefix is still open;
// order and frame count never change.

const bpsStreamBufferLimit = 16 << 20

type bpsPendingFrame struct {
	event string
	data  []byte
	ready bool
}

type bpsStreamDelta struct {
	frame *bpsPendingFrame
	text  string
}

type bpsStreamChannel struct {
	kind, item string
	deltas     []bpsStreamDelta
}

type bpsStreamRedactor struct {
	queue    []*bpsPendingFrame
	channels map[string]*bpsStreamChannel
	bytes    int
}

func newBPSStreamRedactor() *bpsStreamRedactor {
	return &bpsStreamRedactor{channels: map[string]*bpsStreamChannel{}}
}

func bpsStreamChannelKey(event gjson.Result) string {
	return strings.TrimSuffix(strings.TrimSuffix(event.Get("type").String(), ".delta"), ".done") + "\x00" + event.Get("item_id").String() + "\x00" + event.Get("output_index").String() + "\x00" + event.Get("content_index").String() + "\x00" + event.Get("summary_index").String()
}

func (s *bpsStreamRedactor) flushChannel(c *bpsStreamChannel, final bool) error {
	if len(c.deltas) == 0 {
		return nil
	}
	// Tool arguments are caller business data: the host is still hidden, but
	// provider wording inside them is left alone.
	sourceText := !(strings.Contains(c.kind, "arguments") || strings.Contains(c.kind, "input"))
	var builder strings.Builder
	for _, d := range c.deltas {
		builder.WriteString(d.text)
	}
	combined := builder.String()
	cut := len(combined)
	if !final {
		start := max(len(combined)-400, 0)
		for i := start; i < len(combined); i++ {
			if upstreamprivacy.Prefix(combined[i:]) || (sourceText && upstreamprivacy.SourcePrefix(combined[i:])) {
				cut = i
				break
			}
		}
		// Replacements can change length; never move an unfinished protected
		// suffix into a ready frame.
		if cut < len(combined) {
			return nil
		}
	}
	rewritten := upstreamprivacy.Text(combined[:cut])
	if sourceText {
		rewritten = upstreamprivacy.SourceText(rewritten)
	}
	updated := rewritten + combined[cut:]
	consumed, emitted := 0, 0
	for i := range c.deltas {
		d := &c.deltas[i]
		end := consumed + len(d.text)
		if end > len(updated) || i == len(c.deltas)-1 {
			end = len(updated)
		}
		for end > consumed && end < len(updated) && !utf8.RuneStart(updated[end]) {
			end--
		}
		text := updated[consumed:end]
		if text != d.text {
			data, err := sjson.SetBytes(d.frame.data, "delta", text)
			if err != nil {
				return err
			}
			d.frame.data, d.text = data, text
		}
		d.frame.ready = true
		emitted = i + 1
		consumed = end
	}
	c.deltas = append([]bpsStreamDelta(nil), c.deltas[emitted:]...)
	return nil
}

// push queues one projected frame and returns the frames now ready.
func (s *bpsStreamRedactor) push(eventName string, data []byte) ([]plugins.SSEFrame, error) {
	event := gjson.ParseBytes(data)
	kind := event.Get("type").String()
	delta := event.Get("delta")
	isDelta := strings.HasSuffix(kind, ".delta") && delta.Type == gjson.String
	// Non-delta fields are complete in this frame and redacted directly; the
	// delta itself is only redacted after joining its channel.
	data = upstreamprivacy.Bytes(data)
	if isDelta {
		var err error
		if data, err = sjson.SetBytes(data, "delta", delta.String()); err != nil {
			return nil, err
		}
	}
	if kind == "response.in_progress" && len(s.queue) > 0 {
		// A keepalive carries no content, so it may overtake frames held for
		// an open prefix instead of waiting behind them.
		return append(s.drain(), plugins.SSEFrame{Event: eventName, Data: data}), nil
	}
	entry := &bpsPendingFrame{event: eventName, data: data, ready: true}
	s.queue = append(s.queue, entry)
	s.bytes += len(data)
	if s.bytes > bpsStreamBufferLimit {
		return nil, errors.New("BPS stream redaction buffer exceeded")
	}
	if isDelta {
		key := bpsStreamChannelKey(event)
		c := s.channels[key]
		if c == nil {
			c = &bpsStreamChannel{kind: kind, item: event.Get("item_id").String()}
			s.channels[key] = c
		}
		entry.ready = false
		c.deltas = append(c.deltas, bpsStreamDelta{entry, delta.String()})
		if err := s.flushChannel(c, false); err != nil {
			return nil, err
		}
	} else if strings.HasSuffix(kind, ".done") {
		if c := s.channels[bpsStreamChannelKey(event)]; c != nil {
			if err := s.flushChannel(c, true); err != nil {
				return nil, err
			}
			delete(s.channels, bpsStreamChannelKey(event))
		}
		if kind == "response.output_item.done" {
			for key, c := range s.channels {
				if c.item == event.Get("item.id").String() {
					if err := s.flushChannel(c, true); err != nil {
						return nil, err
					}
					delete(s.channels, key)
				}
			}
		}
	}
	switch kind {
	case "response.completed", "response.done", "response.failed", "response.incomplete", "error":
		if err := s.flushAll(); err != nil {
			return nil, err
		}
	}
	if string(data) == "[DONE]" {
		if err := s.flushAll(); err != nil {
			return nil, err
		}
	}
	return s.drain(), nil
}

func (s *bpsStreamRedactor) flushAll() error {
	for key, c := range s.channels {
		if err := s.flushChannel(c, true); err != nil {
			return err
		}
		delete(s.channels, key)
	}
	return nil
}

func (s *bpsStreamRedactor) drain() []plugins.SSEFrame {
	var out []plugins.SSEFrame
	count := 0
	for _, entry := range s.queue {
		if !entry.ready {
			break
		}
		out = append(out, plugins.SSEFrame{Event: entry.event, Data: entry.data})
		s.bytes -= len(entry.data)
		count++
	}
	s.queue = append([]*bpsPendingFrame(nil), s.queue[count:]...)
	return out
}

func (s *bpsStreamRedactor) finish() ([]plugins.SSEFrame, error) {
	if err := s.flushAll(); err != nil {
		return nil, err
	}
	return s.drain(), nil
}
