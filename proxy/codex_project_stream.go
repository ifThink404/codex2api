package proxy

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/codex2api/auth"
	"github.com/codex2api/internal/upstreamprivacy"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func projectTextDelta(kind string) bool {
	if !strings.HasSuffix(kind, ".delta") {
		return false
	}
	return strings.Contains(kind, "text") || strings.Contains(kind, "arguments") || strings.Contains(kind, "input") || strings.Contains(kind, "refusal") || strings.Contains(kind, "code") || strings.Contains(kind, "transcript")
}

type projectPendingFrame struct {
	frame, data []byte
	ready       bool
	size        int
}
type projectDelta struct {
	frame *projectPendingFrame
	text  string
}
type projectDeltaChannel struct {
	kind, item string
	deltas     []projectDelta
}
type projectStreamBuffer struct {
	domainGuard bool
	ctx         context.Context
	account     *auth.Account
	queue       []*projectPendingFrame
	channels    map[string]*projectDeltaChannel
	bytes       int
}

func projectChannelKey(event gjson.Result) string {
	return strings.TrimSuffix(strings.TrimSuffix(event.Get("type").String(), ".delta"), ".done") + "\x00" + event.Get("item_id").String() + "\x00" + event.Get("output_index").String() + "\x00" + event.Get("content_index").String() + "\x00" + event.Get("summary_index").String()
}

func projectUUIDPrefix(text string) bool {
	if len(text) == 0 || len(text) > 216 {
		return false
	}
	position := 0
	for offset := 0; offset < len(text); offset++ {
		if position >= 36 {
			return false
		}
		b := text[offset]
		if b == '\\' {
			rest := text[offset:]
			if len(rest) < 6 {
				pattern := `\u0000`
				for j := 1; j < len(rest); j++ {
					if j < 2 {
						if rest[j] != pattern[j] {
							return false
						}
					} else if !strings.ContainsRune("0123456789abcdefABCDEF", rune(rest[j])) {
						return false
					}
				}
				return true
			}
			if rest[1] != 'u' {
				return false
			}
			n, e := strconv.ParseUint(rest[2:6], 16, 8)
			if e != nil {
				return false
			}
			b = byte(n)
			offset += 5
		}
		i := position
		position++
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if b != '-' {
				return false
			}
		} else if !(b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F') {
			return false
		}
	}
	return true
}

func (s *projectStreamBuffer) flushChannel(c *projectDeltaChannel, final bool) error {
	if len(c.deltas) == 0 {
		return nil
	}
	structured := strings.Contains(c.kind, "arguments") || strings.Contains(c.kind, "input")
	state := projectIdentityFrom(s.ctx)
	projectActive := state != nil && state.active
	if s.domainGuard && !projectActive {
		structured = false
	}
	if structured && !final {
		return nil
	}
	var builder strings.Builder
	for _, d := range c.deltas {
		builder.WriteString(d.text)
	}
	combined := builder.String()
	cut := len(combined)
	if !final {
		start := len(combined) - 216
		if start < 0 {
			start = 0
		}
		for i := start; i < len(combined); i++ {
			if (projectActive && projectUUIDPrefix(combined[i:])) || (s.domainGuard && upstreamprivacy.Prefix(combined[i:])) {
				cut = i
				break
			}
		}
	}
	part := combined[:cut]
	var rewritten string
	var err error
	if structured && gjson.Valid(part) {
		encoded, err := restoreProjectResponse(s.ctx, s.account, []byte(part))
		if err != nil {
			return err
		}
		rewritten = string(encoded)
	} else {
		rewritten, err = restoreProjectText(s.ctx, s.account, part, "stream."+c.kind)
		if err != nil {
			return err
		}
	}
	if s.domainGuard {
		rewritten = upstreamprivacy.Text(rewritten)
	}
	updated := rewritten + combined[cut:]
	// Text UUID substitutions are equal length. Structured JSON may change
	// escaping; repartition only when the whole argument stream is complete.
	consumed, emitted, sourceOffset := 0, 0, 0
	for i := range c.deltas {
		d := &c.deltas[i]
		sourceEnd := sourceOffset + len(d.text)
		end := consumed + len(d.text)
		if end > len(updated) || final && i == len(c.deltas)-1 {
			end = len(updated)
		}
		for end > consumed && end < len(updated) && !utf8.RuneStart(updated[end]) {
			end--
		}
		text := updated[consumed:end]
		if text != d.text {
			d.frame.data, err = sjson.SetBytes(d.frame.data, "delta", text)
			if err != nil {
				return err
			}
			d.text = text
		}
		if final || sourceEnd <= cut {
			d.frame.ready = true
			emitted = i + 1
		}
		consumed = end
		sourceOffset = sourceEnd
	}
	if emitted > 0 {
		c.deltas = append([]projectDelta(nil), c.deltas[emitted:]...)
	}
	return nil
}

// Hold original events while a UUID straddles deltas. Repartition restored text
// into those same events: sequence numbers/order are never invented or changed.
func (s *projectStreamBuffer) push(frame, data []byte) ([]byte, error) {
	entry := &projectPendingFrame{frame: frame, data: data, ready: true, size: len(frame)}
	s.queue = append(s.queue, entry)
	s.bytes += len(frame)
	if s.bytes > 16<<20 {
		return nil, errors.New("project identity stream buffer exceeded")
	}
	event := gjson.ParseBytes(data)
	kind := event.Get("type").String()
	if (projectTextDelta(kind) || s.domainGuard && strings.HasSuffix(kind, ".delta")) && event.Get("delta").Type == gjson.String {
		key := projectChannelKey(event)
		c := s.channels[key]
		if c == nil {
			c = &projectDeltaChannel{kind: kind, item: event.Get("item_id").String()}
			s.channels[key] = c
		}
		entry.ready = false
		c.deltas = append(c.deltas, projectDelta{entry, event.Get("delta").String()})
		if err := s.flushChannel(c, false); err != nil {
			return nil, err
		}
	} else if strings.HasSuffix(kind, ".done") {
		if c := s.channels[projectChannelKey(event)]; c != nil {
			if err := s.flushChannel(c, true); err != nil {
				return nil, err
			}
			delete(s.channels, projectChannelKey(event))
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
func (s *projectStreamBuffer) flushAll() error {
	for key, c := range s.channels {
		if err := s.flushChannel(c, true); err != nil {
			return err
		}
		delete(s.channels, key)
	}
	return nil
}
func (s *projectStreamBuffer) drain() []byte {
	var output []byte
	count := 0
	for _, entry := range s.queue {
		if !entry.ready {
			break
		}
		output = append(output, rewriteSSEFrame(entry.frame, entry.data)...)
		s.bytes -= entry.size
		count++
	}
	s.queue = append([]*projectPendingFrame(nil), s.queue[count:]...)
	return output
}
func (s *projectStreamBuffer) finish() ([]byte, error) {
	if err := s.flushAll(); err != nil {
		return nil, err
	}
	return s.drain(), nil
}
