package plugins

import (
	"bufio"
	"bytes"
	"errors"
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
		var finish func() ([]SSEFrame, error)
		if f, ok := t.(SSEFinisher); ok {
			finish = func() ([]SSEFrame, error) { return f.FinishSSE(env) }
		}
		resp.Body = newSSETransformReader(resp.Body, func(event string, data []byte) ([]SSEFrame, error) {
			return t.TransformSSEFrame(env, event, data)
		}, finish)
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

type sseFrameFunc func(event string, data []byte) ([]SSEFrame, error)

// sseTransformReader re-frames an SSE stream. A frame ends at a blank line;
// frames without data lines, and frames the transformer returns unchanged,
// are passed through byte for byte. A transformer error ends the stream with
// that error.
type sseTransformReader struct {
	src      io.ReadCloser
	br       *bufio.Reader
	fn       sseFrameFunc
	finish   func() ([]SSEFrame, error)
	frame    bytes.Buffer
	out      bytes.Buffer
	err      error
	finished bool
}

func newSSETransformReader(src io.ReadCloser, fn sseFrameFunc, finish func() ([]SSEFrame, error)) *sseTransformReader {
	return &sseTransformReader{src: src, br: bufio.NewReader(src), fn: fn, finish: finish}
}

func (r *sseTransformReader) Read(p []byte) (int, error) {
	for r.out.Len() == 0 && r.err == nil {
		line, err := r.br.ReadBytes('\n')
		if len(line) > 0 {
			r.frame.Write(line)
			if len(bytes.TrimRight(line, "\r\n")) == 0 {
				if ferr := r.flushFrame(); ferr != nil {
					r.err = ferr
					break
				}
			}
		}
		if err != nil {
			if r.frame.Len() > 0 {
				if ferr := r.flushFrame(); ferr != nil {
					err = ferr
				}
			}
			if errors.Is(err, io.EOF) && !r.finished && r.finish != nil {
				r.finished = true
				frames, ferr := r.finish()
				r.writeFrames(frames)
				if ferr != nil {
					err = ferr
				}
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

func (r *sseTransformReader) writeFrames(frames []SSEFrame) {
	for _, frame := range frames {
		if frame.Event != "" {
			r.out.WriteString("event: ")
			r.out.WriteString(frame.Event)
			r.out.WriteByte('\n')
		}
		for _, line := range bytes.Split(frame.Data, []byte("\n")) {
			r.out.WriteString("data: ")
			r.out.Write(line)
			r.out.WriteByte('\n')
		}
		r.out.WriteByte('\n')
	}
}

func (r *sseTransformReader) flushFrame() error {
	raw := r.frame.Bytes()
	defer r.frame.Reset()
	var event string
	var data [][]byte
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
		}
	}
	if !hasData {
		r.out.Write(raw)
		return nil
	}
	joined := bytes.Join(data, []byte("\n"))
	frames, err := r.fn(event, joined)
	if err != nil {
		return err
	}
	if len(frames) == 1 && frames[0].Event == event && bytes.Equal(frames[0].Data, joined) {
		r.out.Write(raw)
		return nil
	}
	r.writeFrames(frames)
	return nil
}

// ApplyResponseTransformer applies t to resp exactly as Route.Execute does.
// Exposed for plugin tests.
func ApplyResponseTransformer(resp *http.Response, t ResponseTransformer, env *ReqEnv) error {
	return wrapResponse(resp, t, env)
}
