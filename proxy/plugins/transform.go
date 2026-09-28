package plugins

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// wrapResponse applies a ResponseTransformer to resp in place: headers now,
// the body lazily. SSE bodies are rewritten frame by frame as core reads
// them; other bodies are read, transformed and replaced up front.
func wrapResponse(resp *http.Response, t ResponseTransformer, env *ReqEnv) error {
	if resp.Header == nil {
		resp.Header = http.Header{}
	}
	t.FilterHeaders(env, resp.Header)
	if resp.Body == nil {
		return nil
	}
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		resp.Body = newSSETransformReader(resp.Body, func(event string, data []byte) (string, []byte, bool) {
			return t.TransformSSEFrame(env, event, data)
		})
		resp.ContentLength = -1
		resp.Header.Del("Content-Length")
		return nil
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return err
	}
	out, err := t.TransformJSON(env, resp.StatusCode, body)
	if err != nil {
		return err
	}
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	if resp.Header.Get("Content-Length") != "" {
		resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
	}
	return nil
}

type sseFrameFunc func(event string, data []byte) (string, []byte, bool)

// sseTransformReader re-frames an SSE stream. A frame ends at a blank line;
// frames without data lines, and frames the transformer leaves unchanged, are
// passed through byte for byte.
type sseTransformReader struct {
	src   io.ReadCloser
	br    *bufio.Reader
	fn    sseFrameFunc
	frame bytes.Buffer
	out   bytes.Buffer
	err   error
}

func newSSETransformReader(src io.ReadCloser, fn sseFrameFunc) *sseTransformReader {
	return &sseTransformReader{src: src, br: bufio.NewReader(src), fn: fn}
}

func (r *sseTransformReader) Read(p []byte) (int, error) {
	for r.out.Len() == 0 && r.err == nil {
		line, err := r.br.ReadBytes('\n')
		if len(line) > 0 {
			r.frame.Write(line)
			if len(bytes.TrimRight(line, "\r\n")) == 0 {
				r.flushFrame()
			}
		}
		if err != nil {
			if r.frame.Len() > 0 {
				r.flushFrame()
			}
			r.err = err
		}
	}
	if r.out.Len() > 0 {
		return r.out.Read(p)
	}
	return 0, r.err
}

func (r *sseTransformReader) Close() error { return r.src.Close() }

func (r *sseTransformReader) flushFrame() {
	raw := r.frame.Bytes()
	defer r.frame.Reset()
	var event string
	var data [][]byte
	var other [][]byte
	hasData := false
	for _, line := range bytes.SplitAfter(raw, []byte("\n")) {
		trimmed := bytes.TrimRight(line, "\r\n")
		switch {
		case len(trimmed) == 0:
		case bytes.HasPrefix(trimmed, []byte("event:")):
			event = strings.TrimSpace(string(trimmed[len("event:"):]))
		case bytes.HasPrefix(trimmed, []byte("data:")):
			hasData = true
			value := trimmed[len("data:"):]
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
			data = append(data, value)
		default:
			other = append(other, trimmed)
		}
	}
	if !hasData {
		r.out.Write(raw)
		return
	}
	joined := bytes.Join(data, []byte("\n"))
	outEvent, outData, drop := r.fn(event, joined)
	if drop {
		return
	}
	if outEvent == event && bytes.Equal(outData, joined) {
		r.out.Write(raw)
		return
	}
	for _, line := range other {
		r.out.Write(line)
		r.out.WriteByte('\n')
	}
	if outEvent != "" {
		r.out.WriteString("event: ")
		r.out.WriteString(outEvent)
		r.out.WriteByte('\n')
	}
	for _, line := range bytes.Split(outData, []byte("\n")) {
		r.out.WriteString("data: ")
		r.out.Write(line)
		r.out.WriteByte('\n')
	}
	r.out.WriteByte('\n')
}
