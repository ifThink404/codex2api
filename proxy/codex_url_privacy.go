package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type codexURLPrivacyKey struct{}

type codexURLChange struct {
	Original string   `json:"original"`
	Outbound string   `json:"outbound"`
	Sources  []string `json:"sources"`
}

type codexURLDiagnostic struct {
	Version          string           `json:"version"`
	Changes          []codexURLChange `json:"changes"`
	Omitted          int              `json:"omitted,omitempty"`
	AmbiguousRestore bool             `json:"ambiguous_restore,omitempty"`
	RewriteCount     int              `json:"rewrite_count"`
	RestoreCount     int              `json:"restore_count"`
}

// Official URLs are intentionally shared. The inverse is request-local, never
// a global official-URL -> customer-URL dictionary. Retries reuse this snapshot.
type codexURLPrivacy struct {
	mu          sync.Mutex
	accountID   int64
	accountHash string
	target      string
	forward     map[string]string
	protected   map[string]bool
	sources     map[string][]string
	headers     map[string]bool
	initialized bool
	rewritten   int
	restored    int
}

func codexURLState(ctx context.Context, account *auth.Account) *codexURLPrivacy {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(codexURLPrivacyKey{}).(*codexURLPrivacy)
	if s != nil && account != nil && (s.accountID != account.ID() || s.accountHash != turnStateAccountHash(account)) {
		return nil
	}
	return s
}

func codexURLField(key string) bool {
	switch privacyField(key) {
	case "baseurl", "openaibaseurl", "gatewayurl", "xbaseurl", "xopenaibaseurl", "xgatewayurl":
		return true
	}
	return false
}

func codexURLOpaque(key string) bool {
	if codexCredentialMetadataField(key) || isTurnStateField(key) {
		return true
	}
	switch privacyField(key) {
	case "signature", "encryptedcontent", "encryptedfunctionargs":
		return true
	}
	return false
}

func codexURLHeaderOpaque(key string) bool {
	if codexURLOpaque(key) {
		return true
	}
	switch strings.ToLower(key) {
	case "host", ":authority", "content-length", "content-encoding", "content-type", "connection", "upgrade", "sec-websocket-key", "sec-websocket-accept":
		return true
	}
	return false
}

func codexURLPrivacyError() error {
	return codexAccountIdentityError("入口地址元数据无法安全处理，请检查地址字段及嵌套内容。")
}

// Only call this on protocol metadata, never on a full Responses envelope.
// Preserve duplicate keys, numbers and unchanged bytes for existing validators.
func walkCodexURLMetadata(raw []byte, key, path string, depth int, fn func(string, string, string) (string, error)) ([]byte, error) {
	if depth > 32 {
		return nil, codexURLPrivacyError()
	}
	v := gjson.ParseBytes(raw)
	if v.Type == gjson.String {
		text := v.String()
		if gjson.Valid(text) && (gjson.Parse(text).IsObject() || gjson.Parse(text).IsArray()) {
			next, err := walkCodexURLMetadata([]byte(text), key, path+"::<json>", depth+1, fn)
			if err != nil {
				return nil, err
			}
			if string(next) == text {
				return raw, nil
			}
			return json.Marshal(string(next))
		}
		next, err := fn(key, text, path)
		if err != nil {
			return nil, err
		}
		if next == text {
			return raw, nil
		}
		return json.Marshal(next)
	}
	if !v.IsObject() && !v.IsArray() {
		return raw, nil
	}
	var out bytes.Buffer
	if v.IsObject() {
		out.WriteByte('{')
	} else {
		out.WriteByte('[')
	}
	first, changed := true, false
	var failure error
	keys := map[string]string{}
	v.ForEach(func(k, child gjson.Result) bool {
		if !first {
			out.WriteByte(',')
		}
		first = false
		childKey, childPath := key, path+"[]"
		if v.IsObject() {
			childKey, childPath = k.String(), path+"."+k.String()
			nextKey, err := fn("", k.String(), path+"::<key>")
			if err != nil {
				failure = err
				return false
			}
			if previous, exists := keys[nextKey]; exists && previous != k.String() {
				failure = codexURLPrivacyError()
				return false
			}
			keys[nextKey] = k.String()
			if nextKey == k.String() {
				out.WriteString(k.Raw)
			} else {
				b, _ := json.Marshal(nextKey)
				out.Write(b)
				changed = true
			}
			out.WriteByte(':')
		}
		next := []byte(child.Raw)
		if !codexURLOpaque(childKey) {
			next, failure = walkCodexURLMetadata(next, childKey, childPath, depth+1, fn)
			if failure != nil {
				return false
			}
		}
		changed = changed || string(next) != child.Raw
		out.Write(next)
		return true
	})
	if failure != nil {
		return nil, failure
	}
	if !changed {
		return raw, nil
	}
	if v.IsObject() {
		out.WriteByte('}')
	} else {
		out.WriteByte(']')
	}
	return out.Bytes(), nil
}

func (s *codexURLPrivacy) enroll(key, value, path string) (string, error) {
	if !codexURLField(key) {
		return value, nil
	}
	if len(value) > 4096 {
		return "", codexURLPrivacyError()
	}
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
		return value, nil
	}
	if len(s.forward) >= 64 {
		if _, exists := s.forward[value]; !exists {
			return "", codexURLPrivacyError()
		}
	}
	target := s.target
	for _, suffix := range []string{"/responses/compact", "/responses"} {
		if strings.HasSuffix(strings.TrimSuffix(u.Path, "/"), suffix) {
			target += suffix
			break
		}
	}
	if strings.HasSuffix(u.Path, "/") {
		target += "/"
	}
	// Already-mapped values from this attempt are not new customer addresses.
	if s.initialized && s.protected[value] {
		return value, nil
	}
	s.forward[value] = target
	s.protected[target] = true
	s.noteSource(value, path)
	return value, nil
}

func urlReplacementBoundary(text string, start, end int) bool {
	if start > 0 {
		b := text[start-1]
		if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' {
			return false
		}
	}
	if end == len(text) || text[end-1] == '/' {
		return true
	}
	r, _ := utf8.DecodeRuneInString(text[end:])
	return r >= utf8.RuneSelf || unicode.IsSpace(r) || strings.ContainsRune("/?#&\"'`<>()[]{},;!", r)
}

func replaceCodexURLs(text string, pairs map[string]string) string {
	keys := make([]string, 0, len(pairs))
	for key := range pairs {
		if key != "" {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) == len(keys[j]) {
			return keys[i] < keys[j]
		}
		return len(keys[i]) > len(keys[j])
	})
	var out strings.Builder
	for pos := 0; pos < len(text); {
		matched := false
		for _, key := range keys {
			if strings.HasPrefix(text[pos:], key) && urlReplacementBoundary(text, pos, pos+len(key)) {
				// An ambiguous longer prefix blocks a shorter inverse too.
				if pairs[key] == "" {
					out.WriteString(key)
				} else {
					out.WriteString(pairs[key])
				}
				pos += len(key)
				matched = true
				break
			}
		}
		if !matched {
			out.WriteByte(text[pos])
			pos++
		}
	}
	return out.String()
}

func (s *codexURLPrivacy) noteSource(original, path string) {
	path = strings.ReplaceAll(path, original, "<url>")
	path = truncateURLDiagnostic(path, 160)
	for _, existing := range s.sources[original] {
		if existing == path {
			return
		}
	}
	if len(s.sources[original]) < 3 {
		s.sources[original] = append(s.sources[original], path)
	}
}

func truncateURLDiagnostic(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	for limit > 0 && !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit] + "…"
}

func (s *codexURLPrivacy) rewrite(_ string, value, path string) (string, error) {
	next := replaceCodexURLs(value, s.forward)
	if next != value {
		s.protected[next] = true
		s.rewritten++
		for original := range s.forward {
			if strings.Contains(value, original) {
				s.noteSource(original, path)
			}
		}
	}
	return next, nil
}

func codexURLProtected(ctx context.Context, account *auth.Account, value string) bool {
	s := codexURLState(ctx, account)
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.protected[value]
}

// Collect explicit address fields before schema cleanup, then rewrite only the
// metadata copy. Host, endpoint selection and business payloads are untouched.
func PrepareCodexURLPrivacy(ctx context.Context, account *auth.Account, body []byte, headers http.Header) (context.Context, []byte, http.Header, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if account == nil {
		return ctx, body, headers, nil
	}
	s := codexURLState(ctx, account)
	if s == nil {
		target := "https://chatgpt.com/backend-api/codex"
		if account.IsRelayStyle() {
			target = "https://api.openai.com/v1"
		}
		s = &codexURLPrivacy{accountID: account.ID(), accountHash: turnStateAccountHash(account), target: target, forward: map[string]string{}, protected: map[string]bool{}, sources: map[string][]string{}, headers: map[string]bool{}}
		ctx = context.WithValue(ctx, codexURLPrivacyKey{}, s)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	enroll := func(key, value, path string) (string, error) {
		original := value
		// Previous releases used generic metadata aliases. Resolve only within
		// the current owner/account binding; the old response mapper stays valid.
		if codexURLField(key) && strings.HasPrefix(value, "meta_") {
			if db, binding := protocolIdentityBinding(ctx, account); db != nil {
				pair, found, err := db.ReadCodexProtocolPair(ctx, binding, "metadata", value, false)
				if err != nil {
					return "", err
				}
				if found {
					value = pair.Public
				}
			}
		}
		_, err := s.enroll(key, value, path)
		if original != value && s.forward[value] != "" {
			s.forward[original] = s.forward[value]
		}
		return original, err
	}
	for _, field := range []string{"client_metadata", "metadata"} {
		if v := gjson.GetBytes(body, field); v.Exists() {
			if _, err := walkCodexURLMetadata([]byte(v.Raw), "", field, 0, enroll); err != nil {
				return ctx, nil, nil, err
			}
		}
	}
	allHeaders := headers.Clone()
	if allHeaders == nil {
		allHeaders = make(http.Header)
	}
	for name, value := range account.GetCustomHeaders() {
		allHeaders.Add(name, value)
	}
	for name, values := range allHeaders {
		if codexURLHeaderOpaque(name) {
			continue
		}
		for _, value := range values {
			raw, _ := json.Marshal(value)
			if _, err := walkCodexURLMetadata(raw, name, "headers."+name, 0, enroll); err != nil {
				return ctx, nil, nil, err
			}
		}
	}
	s.initialized = true
	for _, field := range []string{"client_metadata", "metadata"} {
		if v := gjson.GetBytes(body, field); v.Exists() {
			next, err := walkCodexURLMetadata([]byte(v.Raw), "", field, 0, s.rewrite)
			if err != nil {
				return ctx, nil, nil, err
			}
			if string(next) != v.Raw {
				body, err = sjson.SetRawBytes(body, field, next)
				if err != nil {
					return ctx, nil, nil, err
				}
			}
		}
	}
	updated, err := s.rewriteHeaders(headers)
	s.publish(ctx)
	return ctx, body, updated, err
}

func (s *codexURLPrivacy) rewriteHeaders(headers http.Header) (http.Header, error) {
	out := headers.Clone()
	for name, values := range out {
		if codexURLHeaderOpaque(name) {
			continue
		}
		for i, value := range values {
			raw, _ := json.Marshal(value)
			next, err := walkCodexURLMetadata(raw, name, "headers."+name, 0, s.rewrite)
			if err != nil {
				return nil, err
			}
			var text string
			if err := json.Unmarshal(next, &text); err != nil {
				return nil, err
			}
			out[name][i] = text
			if text != value {
				s.headers[strings.ToLower(name)] = true
			}
		}
	}
	return out, nil
}

// Run after account custom headers and all other header builders.
func FinalizeCodexURLHeaders(ctx context.Context, headers http.Header) (http.Header, error) {
	s := codexURLState(ctx, nil)
	if s == nil {
		return headers, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out, err := s.rewriteHeaders(headers)
	s.publish(ctx)
	return out, err
}

func (s *codexURLPrivacy) reverse() (map[string]string, bool) {
	pairs, ambiguous := map[string]string{}, false
	for original, outbound := range s.forward {
		if strings.HasPrefix(original, "meta_") {
			continue
		}
		if previous, exists := pairs[outbound]; exists && previous != original {
			pairs[outbound] = ""
			ambiguous = true
		} else if !exists {
			pairs[outbound] = original
		}
	}
	return pairs, ambiguous
}

func restoreCodexURLMetadata(ctx context.Context, account *auth.Account, raw []byte) ([]byte, error) {
	s := codexURLState(ctx, account)
	if s == nil {
		return raw, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pairs, _ := s.reverse()
	out, err := walkCodexURLMetadata(raw, "", "response.metadata", 0, func(_ string, value, _ string) (string, error) {
		next := replaceCodexURLs(value, pairs)
		if next != value {
			s.restored++
		}
		return next, nil
	})
	s.publish(ctx)
	return out, err
}

func restoreCodexURLHeaders(ctx context.Context, account *auth.Account, headers http.Header) error {
	s := codexURLState(ctx, account)
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pairs, _ := s.reverse()
	for name, values := range headers {
		if !s.headers[strings.ToLower(name)] || codexURLHeaderOpaque(name) {
			continue
		}
		for i, value := range values {
			raw, _ := json.Marshal(value)
			next, err := walkCodexURLMetadata(raw, "", "response.headers", 0, func(_ string, value, _ string) (string, error) {
				next := replaceCodexURLs(value, pairs)
				if next != value {
					s.restored++
				}
				return next, nil
			})
			if err != nil {
				return err
			}
			if err := json.Unmarshal(next, &headers[name][i]); err != nil {
				return err
			}
		}
	}
	s.publish(ctx)
	return nil
}

func (s *codexURLPrivacy) publish(ctx context.Context) {
	observer := UpstreamTransportObserver(ctx)
	if observer == nil || len(s.forward) == 0 || observer.attempt.accountID != s.accountID {
		return
	}
	keys := make([]string, 0, len(s.forward))
	for key := range s.forward {
		if !strings.HasPrefix(key, "meta_") && s.forward[key] != key {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	d := &codexURLDiagnostic{Version: "url-request-v1", RewriteCount: s.rewritten, RestoreCount: s.restored}
	_, d.AmbiguousRestore = s.reverse()
	for _, key := range keys {
		if len(d.Changes) == 8 {
			d.Omitted++
			continue
		}
		original := key
		if u, err := url.Parse(key); err == nil {
			u.User = nil
			u.RawQuery = ""
			u.Fragment = ""
			original = u.String()
		}
		original = truncateURLDiagnostic(original, 256)
		d.Changes = append(d.Changes, codexURLChange{Original: original, Outbound: s.forward[key], Sources: append([]string(nil), s.sources[key]...)})
	}
	observer.updateOutboundIdentity(func(identity *outboundIdentityDiagnostic) { identity.URLMapping = d })
}
