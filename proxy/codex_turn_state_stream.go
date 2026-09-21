package proxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

var errTurnStateMapping = errors.New("turn-state mapping unavailable")

func finishTurnStateResponse(ctx context.Context, account *auth.Account, response **http.Response, requestErr *error) {
	if *response == nil || preserveCodexAccountTestResponse(ctx, account) {
		return
	}
	if err := maskTurnStateResponse(ctx, account, *response); err != nil {
		if (*response).Body != nil {
			_ = (*response).Body.Close()
		}
		*response = nil
		*requestErr = ErrInternalError("会话状态暂时无法处理，请稍后重试。", errTurnStateMapping)
	}
}

// This is applied to the shared HTTP response abstraction, including WS upstream
// frames converted to SSE. It never invents a metadata event or token.
func maskTurnStateResponse(ctx context.Context, account *auth.Account, response *http.Response) error {
	s := turnStateSessionFrom(ctx)
	if response == nil {
		return nil
	}
	if err := restoreCodexURLHeaders(ctx, account, response.Header); err != nil {
		return err
	}
	observeUsageTurnState(ctx, "")
	var err error
	response.Header, err = rewriteTurnStateHeaders(response.Header, "response_header", func(value, carrier string) (string, error) {
		return maskResponseTurnState(ctx, account, value, carrier)
	})
	if err != nil {
		return err
	}
	if response.Body != nil && strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		response.Body = &turnStateStream{body: response.Body, reader: bufio.NewReaderSize(response.Body, 32*1024), ctx: ctx, account: account, state: s}
		response.ContentLength = -1
		response.Header.Del("Content-Length")
	} else if response.Body != nil {
		// Lazy reads keep response/header timing and the handler's cancellation
		// watchdog intact, even when an upstream omits Content-Type.
		response.Body = &responsePrivacyBody{body: response.Body, ctx: ctx, account: account, statusCode: response.StatusCode}
		response.ContentLength = -1
		response.Header.Del("Content-Length")
	}
	return nil
}

type responsePrivacyBody struct {
	statusCode int
	body       io.ReadCloser
	ctx        context.Context
	account    *auth.Account
	reader     io.Reader
	err        error
}

func (r *responsePrivacyBody) Close() error { return r.body.Close() }
func (r *responsePrivacyBody) observeUpstreamEvent(event string, payload []byte) {
	if observer, ok := r.body.(interface{ observeUpstreamEvent(string, []byte) }); ok {
		observer.observeUpstreamEvent(event, payload)
	}
}
func (r *responsePrivacyBody) finishObservedSSE(terminal bool) error {
	if observer, ok := r.body.(interface{ finishObservedSSE(bool) error }); ok {
		return observer.finishObservedSSE(terminal)
	}
	return nil
}
func (r *responsePrivacyBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.err != nil {
		return 0, r.err
	}
	if r.reader == nil {
		buffer := bufio.NewReader(r.body)
		first, err := buffer.Peek(1)
		for skipped := 0; err == nil && bytes.ContainsAny(first, " \r\n\t"); skipped++ {
			if skipped >= 4096 {
				r.err = errors.New("invalid upstream response prefix")
				return 0, r.err
			}
			_, _ = buffer.Discard(1)
			first, err = buffer.Peek(1)
		}
		if err != nil {
			r.err = err
			return 0, err
		}
		if first[0] == ':' || first[0] == 'd' || first[0] == 'e' || first[0] == 'i' {
			// A mislabeled SSE stream is still processed incrementally.
			r.reader = &turnStateStream{body: r.body, reader: buffer, ctx: r.ctx, account: r.account, state: turnStateSessionFrom(r.ctx)}
		} else {
			var original []byte
			if r.statusCode >= 400 && r.statusCode <= 599 {
				// Bound the raw error before buffering/masking it. A limit outside
				// this lazy reader cannot bound an internal ReadAll of the upstream.
				original, err = readAllLimited(buffer, upstreamErrorBodyReadMaxBytes)
			} else {
				original, err = io.ReadAll(buffer)
			}
			if err == nil {
				if !gjson.ValidBytes(original) {
					if r.statusCode >= 400 && r.statusCode <= 599 {
						// Keep the real HTTP failure category, but discard HTML/plain
						// provider diagnostics. Do not misclassify this as a read error.
						original = nil
					} else {
						err = errors.New("invalid upstream response envelope")
					}
				} else {
					original, err = maskResponsePayload(r.ctx, r.account, original, true)
				}
			}
			if err != nil {
				r.err = err
				return 0, err
			}
			r.reader = bytes.NewReader(original)
		}
	}
	return r.reader.Read(p)
}

// stageTurnStateMetadataHeader promotes the first masked metadata token into
// this attempt's response headers. Codex's HTTP client reads turn state from
// HTTP headers, not from SSE metadata. Keep this on the handler goroutine:
// body reads can run in a separate goroutine while retry heartbeats are sent.
// The downstream header is published only when this attempt is actually used.
func stageTurnStateMetadataHeader(ctx context.Context, headers http.Header, event gjson.Result) {
	if headers == nil || headers.Get(codexTurnStateHeader) != "" {
		return
	}
	switch strings.TrimSpace(event.Get("type").String()) {
	case "response.metadata", "codex.response.metadata", "responsesapi.response.metadata":
	default:
		return
	}
	s := turnStateSessionFrom(ctx)
	if s == nil || s.handler == nil || s.handler.db == nil {
		return
	}
	event.Get("headers").ForEach(func(key, value gjson.Result) bool {
		if !strings.EqualFold(key.String(), codexTurnStateHeader) {
			return true
		}
		if value.IsArray() {
			values := value.Array()
			if len(values) != 1 {
				return true
			}
			value = values[0]
		}
		// Only the alias already issued by the masking layer may be promoted.
		// Never expose raw upstream state through this new response-header path.
		if value.Type == gjson.String && s.handler.db.IsManagedCodexTurnStateAlias(value.String()) {
			headers.Set(codexTurnStateHeader, value.String())
			return false
		}
		return true
	})
}

// Buffer one SSE event, not the response. Mask protocol IDs and header
// dictionaries; leave unrelated events and generated content unchanged.
// The limit also bounds malformed streams without an event separator.
type turnStateStream struct {
	body     io.ReadCloser
	reader   *bufio.Reader
	ctx      context.Context
	account  *auth.Account
	state    *turnStateSession
	pending  []byte
	terminal error
	projects *projectStreamBuffer
}

func (r *turnStateStream) Close() error { return r.body.Close() }
func (r *turnStateStream) observeUpstreamEvent(event string, payload []byte) {
	if observer, ok := r.body.(interface{ observeUpstreamEvent(string, []byte) }); ok {
		observer.observeUpstreamEvent(event, payload)
	}
}
func (r *turnStateStream) finishObservedSSE(terminal bool) error {
	if observer, ok := r.body.(interface{ finishObservedSSE(bool) error }); ok {
		return observer.finishObservedSSE(terminal)
	}
	return nil
}
func (r *turnStateStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(r.pending) == 0 && r.terminal == nil {
		var frame []byte
		lineContinued := false
		for {
			line, err := r.reader.ReadSlice('\n')
			if len(frame)+len(line) > 16<<20 {
				r.terminal = errors.New("upstream SSE event too large")
				break
			}
			frame = append(frame, line...)
			if err == bufio.ErrBufferFull {
				lineContinued = true
				continue
			}
			if err != nil {
				r.terminal = err
				break
			}
			if !lineContinued && (bytes.Equal(line, []byte("\n")) || bytes.Equal(line, []byte("\r\n"))) {
				break
			}
			lineContinued = false
		}
		if r.terminal != nil && r.terminal != io.EOF {
			return 0, r.terminal
		}
		if len(frame) > 0 {
			var err error
			r.pending, err = r.maskFrame(frame)
			if err != nil {
				r.pending = nil
				r.terminal = err
				return 0, err
			}
		}
		if r.terminal == io.EOF && r.projects != nil {
			last, err := r.projects.finish()
			if err != nil {
				r.pending = nil
				r.terminal = err
				return 0, err
			}
			r.pending = append(r.pending, last...)
		}
	}
	if len(r.pending) > 0 {
		n := copy(p, r.pending)
		r.pending = r.pending[n:]
		return n, nil
	}
	return 0, r.terminal
}

func (r *turnStateStream) maskFrame(frame []byte) ([]byte, error) {
	lines := bytes.SplitAfter(frame, []byte("\n"))
	var data []byte
	multiline := false
	for _, line := range lines {
		if bytes.HasPrefix(line, []byte("data:")) {
			part := bytes.TrimSuffix(bytes.TrimSuffix(line[5:], []byte("\n")), []byte("\r"))
			part = bytes.TrimPrefix(part, []byte(" "))
			if data == nil {
				data = part
				continue
			}
			if !multiline {
				data = bytes.Clone(data)
				multiline = true
			}
			data = append(data, '\n')
			data = append(data, part...)
		}
	}
	encoded, err := maskResponsePayload(r.ctx, r.account, data, false)
	if err != nil {
		return nil, err
	}
	if state := projectIdentityFrom(r.ctx); state != nil && state.active {
		if r.projects == nil {
			r.projects = &projectStreamBuffer{ctx: r.ctx, account: r.account, channels: make(map[string]*projectDeltaChannel)}
		}
		return r.projects.push(frame, encoded)
	}
	if bytes.Equal(encoded, data) {
		return frame, nil
	}
	return rewriteSSEFrame(frame, encoded), nil
}

func rewriteSSEFrame(frame, encoded []byte) []byte {
	lines := bytes.SplitAfter(frame, []byte("\n"))
	var output []byte
	replaced := false
	for _, line := range lines {
		if bytes.HasPrefix(line, []byte("data:")) {
			if replaced {
				continue
			}
			output = append(output, []byte("data: ")...)
			output = append(output, encoded...)
			if bytes.HasSuffix(line, []byte("\r\n")) {
				output = append(output, '\r', '\n')
			} else if bytes.HasSuffix(line, []byte("\n")) {
				output = append(output, '\n')
			}
			replaced = true
		} else {
			output = append(output, line...)
		}
	}
	return output
}
