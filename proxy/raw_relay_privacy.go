package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

const rawRelayPrivacyLimit = 16 << 20

var rawRelayEntityHeaders = []string{"Content-Encoding", "Content-Length", "Content-MD5", "Digest", "Content-Digest", "Repr-Digest", "ETag"}

func rawRelayResponseTrailers(resp *http.Response) http.Header {
	headers := rawRelayResponseHeaders(resp.Trailer)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		for _, name := range rawRelayEntityHeaders {
			headers.Del(name)
		}
	}
	return headers
}

func rawRelayPrivateHeader(name string) bool {
	field := privacyField(name)
	// Diagnostic IDs, retry hints and continuation handles remain functional.
	switch field {
	case "requestid", "xrequestid", "xoairequestid", "xopenairequestid", "xclientrequestid", "xcodexturnstate":
		return false
	case "useragent", "xuseragent", "xopenaiaccountid", "xchatgptaccountid", "xaccountid", "xdeviceid", "xinstallationid", "xsessionid", "agentiteration", "originator", "clientprofile":
		return true
	}
	return privateResponseField(name) || bpsSourceField(name) ||
		privateResponseField(strings.TrimPrefix(field, "x")) ||
		strings.HasPrefix(field, "xopenaiinternal") || strings.HasPrefix(field, "xnewapi") ||
		strings.HasPrefix(field, "xcodex2api") || strings.HasPrefix(field, "secauth") ||
		strings.HasPrefix(field, "secchua") || strings.HasSuffix(field, "apikey") ||
		strings.HasSuffix(field, "accountid") || strings.HasSuffix(field, "deviceid") ||
		strings.HasSuffix(field, "installationid") || strings.HasSuffix(field, "organizationid") ||
		strings.HasSuffix(field, "token") || strings.HasSuffix(field, "secret") ||
		strings.HasSuffix(field, "cookie") || strings.HasSuffix(field, "authorization")
}

// Only protocol envelopes are traversed. Model/tool payloads and error objects
// are opaque. In particular, do not run native response-ID aliasing on API relay.
func sanitizeRawRelayJSON(data []byte, control bool, depth int) ([]byte, error) {
	if depth > 64 {
		return nil, errors.New("upstream response nesting too deep")
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return data, nil
	}
	if bytes.Equal(trimmed, []byte("[DONE]")) {
		return data, nil
	}
	if trimmed[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(data, &items); err != nil {
			return nil, errors.New("invalid upstream response JSON")
		}
		changed := false
		for i, item := range items {
			out, err := sanitizeRawRelayJSON(item, control, depth+1)
			if err != nil {
				return nil, err
			}
			changed = changed || !bytes.Equal(out, item)
			items[i] = out
		}
		if changed {
			return json.Marshal(items)
		}
		return data, nil
	}
	if trimmed[0] != '{' {
		return data, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, errors.New("invalid upstream response JSON")
	}
	var kind string
	_ = json.Unmarshal(object["type"], &kind)
	if !control && (kind == "error" || kind == "response.error") {
		return data, nil
	}
	changed := false
	for key, value := range object {
		field := privacyField(key)
		if !control && (field == "error" || ResponseToolErrorField(kind, key) || ResponseOpaquePayloadField(kind, key) ||
			field == "encryptedcontent" || field == "signature" || field == "attestation") {
			continue
		}
		switch field {
		case "metadata", "clientmetadata", "internalchatmessagemetadatapassthrough", "xcodexturnmetadata",
			"taskid", "turnid", "rootturnid", "agentiteration", "sessionid", "threadid", "accountid", "chatgptaccountid", "organizationid", "deviceid", "installationid", "useragent",
			"authorization", "proxyauthorization", "cookie", "setcookie", "accesstoken", "refreshtoken", "idtoken", "apikey", "credentials", "password", "secret":
			delete(object, key)
			changed = true
			continue
		}
		if bpsSourceField(key) || strings.HasPrefix(field, "xopenaiinternal") {
			delete(object, key)
			changed = true
			continue
		}
		var out []byte
		var err error
		if field == "headers" {
			out, err = sanitizeRawRelayHeaderObject(value)
		} else {
			childControl := control
			switch field {
			case "response", "item", "part", "output", "choices", "message", "toolcalls", "function":
			default:
				childControl = true
			}
			out, err = sanitizeRawRelayJSON(value, childControl, depth+1)
		}
		if err != nil {
			return nil, err
		}
		changed = changed || !bytes.Equal(value, out)
		object[key] = out
	}
	if changed {
		return json.Marshal(object)
	}
	return data, nil
}

func sanitizeRawRelayHeaderObject(data []byte) ([]byte, error) {
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil || object == nil {
		return []byte(`{}`), nil
	}
	changed := false
	for key, value := range object {
		var scalar string
		var values []string
		if rawRelayPrivateHeader(key) || (json.Unmarshal(value, &scalar) != nil && json.Unmarshal(value, &values) != nil) {
			delete(object, key)
			changed = true
		}
	}
	if changed {
		return json.Marshal(object)
	}
	return data, nil
}

// HTTP errors intentionally retain their body bytes, including compression.
// Successful JSON/SSE bodies are decoded before filtering; stale entity headers
// cannot describe the sanitized representation.
func prepareRawRelayResponsePrivacy(resp *http.Response) error {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	body, err := rawRelayPrivacyDecodedBody(resp.Body, resp.Header.Get("Content-Encoding"))
	if err != nil {
		return err
	}
	stream := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
	resp.Body = &rawRelayPrivacyBody{body: body, reader: bufio.NewReader(body), stream: stream}
	resp.ContentLength = -1
	for _, name := range rawRelayEntityHeaders {
		resp.Header.Del(name)
		resp.Trailer.Del(name)
	}
	return nil
}

func rawRelayPrivacyDecodedBody(body io.ReadCloser, encoding string) (io.ReadCloser, error) {
	encs := strings.Split(encoding, ",")
	if len(encs) > 4 {
		return nil, errors.New("too many upstream content encodings")
	}
	reader := io.Reader(body)
	closers := []io.Closer{body}
	fail := func() (io.ReadCloser, error) {
		for _, closer := range closers {
			_ = closer.Close()
		}
		return nil, errors.New("unable to decode upstream response")
	}
	for i := len(encs) - 1; i >= 0; i-- {
		switch strings.ToLower(strings.TrimSpace(encs[i])) {
		case "", "identity":
		case "gzip", "x-gzip":
			r, err := gzip.NewReader(reader)
			if err != nil {
				return fail()
			}
			reader, closers = r, append([]io.Closer{r}, closers...)
		case "deflate":
			r, err := zlib.NewReader(reader)
			if err != nil {
				return fail()
			}
			reader, closers = r, append([]io.Closer{r}, closers...)
		case "br":
			reader = brotli.NewReader(reader)
		case "zstd":
			r, err := zstd.NewReader(reader, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(64<<20))
			if err != nil {
				return fail()
			}
			reader, closers = r, append([]io.Closer{r.IOReadCloser()}, closers...)
		default:
			return fail()
		}
	}
	return &chainedReadCloser{Reader: reader, closers: closers}, nil
}

type rawRelayPrivacyBody struct {
	body     io.ReadCloser
	reader   *bufio.Reader
	stream   bool
	skipLF   bool
	pending  []byte
	terminal error
}

func (r *rawRelayPrivacyBody) Close() error { return r.body.Close() }

func (r *rawRelayPrivacyBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(r.pending) == 0 && r.terminal == nil {
		var data []byte
		var err error
		if r.stream {
			data, err = r.readFrame()
		} else {
			data, err = io.ReadAll(io.LimitReader(r.reader, rawRelayPrivacyLimit+1))
			if len(data) > rawRelayPrivacyLimit {
				data = nil
				err = errors.New("upstream JSON response too large")
			}
			if err == nil {
				err = io.EOF
			}
		}
		r.terminal = err
		if len(data) > 0 {
			var filterErr error
			if r.stream {
				r.pending, filterErr = sanitizeRawRelayFrame(data)
			} else {
				r.pending, filterErr = sanitizeRawRelayJSON(data, false, 0)
			}
			if filterErr != nil {
				r.pending = nil
				r.terminal = filterErr
			}
		}
	}
	if len(r.pending) > 0 {
		n := copy(p, r.pending)
		r.pending = r.pending[n:]
		return n, nil
	}
	return 0, r.terminal
}

// SSE accepts LF, CRLF and CR. Never wait for another event just to find out
// whether a CR terminator will be followed by LF in a later network read.
func (r *rawRelayPrivacyBody) readFrame() ([]byte, error) {
	var frame []byte
	lineHasData := false
	for {
		if _, err := r.reader.Peek(1); err != nil {
			return frame, err
		}
		chunk, _ := r.reader.Peek(r.reader.Buffered())
		if r.skipLF {
			r.skipLF = false
			if chunk[0] == '\n' {
				_, _ = r.reader.Discard(1)
				continue
			}
		}
		end := bytes.IndexAny(chunk, "\r\n")
		take := len(chunk)
		if end >= 0 {
			take = end + 1
		}
		if len(frame)+take > rawRelayPrivacyLimit {
			return nil, errors.New("upstream SSE event too large")
		}
		frame = append(frame, chunk[:take]...)
		_, _ = r.reader.Discard(take)
		if end < 0 {
			lineHasData = true
			continue
		}
		blank := !lineHasData && end == 0
		lineHasData = false
		if chunk[end] == '\r' {
			r.skipLF = true
			if r.reader.Buffered() > 0 {
				next, _ := r.reader.Peek(1)
				if next[0] == '\n' {
					if len(frame) == rawRelayPrivacyLimit {
						return nil, errors.New("upstream SSE event too large")
					}
					frame = append(frame, '\n')
					_, _ = r.reader.Discard(1)
					r.skipLF = false
				}
			}
		}
		if blank {
			return frame, nil
		}
	}
}

func sanitizeRawRelayFrame(frame []byte) ([]byte, error) {
	for i, b := range frame {
		if b == '\r' && (i+1 == len(frame) || frame[i+1] != '\n') {
			frame = bytes.ReplaceAll(bytes.ReplaceAll(frame, []byte("\r\n"), []byte("\n")), []byte("\r"), []byte("\n"))
			break
		}
	}
	// A UTF-8 BOM is permitted at the beginning of an SSE stream. It must not
	// hide the first data field from the privacy parser.
	parsedFrame := bytes.TrimPrefix(frame, []byte{0xef, 0xbb, 0xbf})
	var parts [][]byte
	var event string
	for _, line := range bytes.Split(parsedFrame, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("event:")) {
			event = strings.TrimSpace(string(line[6:]))
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			parts = append(parts, bytes.TrimPrefix(bytes.TrimSuffix(line[5:], []byte("\r")), []byte(" ")))
		}
	}
	if len(parts) == 0 {
		return frame, nil
	}
	data := bytes.Join(parts, []byte("\n"))
	if event == "error" || event == "response.error" {
		var envelope struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(data, &envelope) == nil && (envelope.Type == "" || envelope.Type == "error" || envelope.Type == "response.error") {
			return frame, nil
		}
	}
	clean, err := sanitizeRawRelayJSON(data, false, 0)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(clean, data) {
		return frame, nil
	}
	return rewriteSSEFrame(parsedFrame, clean), nil
}
