package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var projectUUIDPattern = regexp.MustCompile(`(?i)(?:[0-9a-f]|\\u00[0-9a-f]{2}){8}(?:-|\\u002d)(?:[0-9a-f]|\\u00[0-9a-f]{2}){4}(?:-|\\u002d)(?:[0-9a-f]|\\u00[0-9a-f]{2}){4}(?:-|\\u002d)(?:[0-9a-f]|\\u00[0-9a-f]{2}){4}(?:-|\\u002d)(?:[0-9a-f]|\\u00[0-9a-f]{2}){12}`)
var projectLabelPattern = regexp.MustCompile(`(?i)(?:project[_ -]?id|项目\s*(?:id|标识))\s*["'` + "`" + `：:=*\s]*([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)

var projectBusinessPaths = []string{"input", "instructions", "tools", "additional_tools", "tool_choice", "text", "metadata", "prompt.variables"}

type projectIdentityContextKey struct{}
type projectIdentityChange struct {
	Original string   `json:"original"`
	Outbound string   `json:"outbound"`
	Sources  []string `json:"sources,omitempty"`
	Replaced int      `json:"replaced"`
	Restored int      `json:"restored"`
}
type projectIdentityDiagnostic struct {
	Version   string                  `json:"version"`
	AccountID int64                   `json:"account_id"`
	ScopeHash string                  `json:"scope_hash"`
	Changes   []projectIdentityChange `json:"changes"`
}
type projectIdentityState struct {
	db        *database.DB
	binding   database.CodexTurnStateBinding
	mu        sync.Mutex
	forward   map[string]string
	reverse   map[string]string
	changes   map[string]*projectIdentityChange
	active    bool
	control   *projectControlSnapshot
	projected map[string]bool
}

func projectIdentityFrom(ctx context.Context) *projectIdentityState {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(projectIdentityContextKey{}).(*projectIdentityState)
	return s
}

func projectIdentityError() error {
	return &Error{Code: "codex_project_identity_unavailable", Type: ErrorTypeServerError, HTTPStatus: http.StatusBadGateway, Message: "项目标识映射暂时不可用，请重试。"}
}

func projectOpaqueField(key string) bool {
	switch privacyField(key) {
	case "encryptedcontent", "encryptedfunctionargs", "signature", "attestation", "xoaiattestation", "authorization", "cookie", "imagedata", "imageurl", "audiourl":
		return true
	}
	return false
}

// Preserve raw numbers, duplicate business keys, and unchanged JSON bytes.
// JSON-encoded text is interpreted only for finding/replacing registered UUIDs.
func rewriteProjectJSON(raw []byte, path string, depth int, rewrite func(string, string) (string, error)) ([]byte, error) {
	if depth > 64 {
		// Do not silently transmit a known value when nesting exceeds the
		// structured walker. Unrelated deep data can remain byte-for-byte intact.
		probe, err := rewrite(string(raw), path+"::<depth-limit>")
		if err != nil {
			return nil, err
		}
		if probe != string(raw) {
			return nil, projectIdentityError()
		}
		return raw, nil
	}
	v := gjson.ParseBytes(raw)
	if v.Type == gjson.String {
		text := v.String()
		if gjson.Valid(text) && (gjson.Parse(text).IsObject() || gjson.Parse(text).IsArray()) {
			next, err := rewriteProjectJSON([]byte(text), path+"::<json>", depth+1, rewrite)
			if err != nil {
				return nil, err
			}
			// Re-scanning the serialized object would rewrite excluded opaque
			// fields such as signatures and encrypted_content a second time.
			if string(next) == text {
				return raw, nil
			}
			return json.Marshal(string(next))
		}
		next, err := rewrite(text, path)
		if err != nil {
			return nil, err
		}
		if next == v.String() {
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
	changed, first, index := false, true, 0
	keySources := make(map[string]string)
	var failure error
	v.ForEach(func(key, child gjson.Result) bool {
		if !first {
			out.WriteByte(',')
		}
		first = false
		childPath := fmt.Sprintf("%s[%d]", path, index)
		index++
		if v.IsObject() {
			nextKey, err := rewrite(key.String(), path+"::<key>")
			if err != nil {
				failure = err
				return false
			}
			if original, exists := keySources[nextKey]; exists && original != key.String() {
				failure = projectIdentityError()
				return false
			}
			keySources[nextKey] = key.String()
			if nextKey != key.String() {
				encoded, _ := json.Marshal(nextKey)
				out.Write(encoded)
				changed = true
			} else {
				out.WriteString(key.Raw)
			}
			out.WriteByte(':')
			childPath = path + "." + key.String()
		}
		next := []byte(child.Raw)
		if !v.IsObject() || !projectOpaqueField(key.String()) {
			next, failure = rewriteProjectJSON(next, childPath, depth+1, rewrite)
		}
		if failure != nil {
			return false
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

// Discovery is intentionally semantic: explicit project fields/objects or a
// project-ID label. A bare UUID is only rewritten after caller-scoped enrollment.
func collectProjectIDs(raw []byte, path string, projectObject bool, depth int, found map[string][]string) {
	if depth > 64 {
		return
	}
	add := func(value, source string) {
		if id, err := uuid.Parse(value); err == nil && len(value) == 36 && id != uuid.Nil {
			found[id.String()] = append(found[id.String()], source)
		}
	}
	v := gjson.ParseBytes(raw)
	if v.Type == gjson.String {
		text := v.String()
		if projectObject {
			add(text, path)
		}
		if gjson.Valid(text) {
			collectProjectIDs([]byte(text), path+"::<json>", projectObject, depth+1, found)
			return
		}
		for _, match := range projectLabelPattern.FindAllStringSubmatch(text, -1) {
			add(match[1], path)
		}
		return
	}
	if v.IsArray() {
		for i, child := range v.Array() {
			collectProjectIDs([]byte(child.Raw), fmt.Sprintf("%s[%d]", path, i), projectObject, depth+1, found)
		}
		return
	}
	if !v.IsObject() {
		return
	}
	v.ForEach(func(key, child gjson.Result) bool {
		name := privacyField(key.String())
		source := path + "." + key.String()
		if projectOpaqueField(key.String()) {
			return true
		}
		if projectControlField(key.String()) != "" || projectObject && name == "id" {
			if child.Type == gjson.String {
				add(child.String(), source)
			}
		}
		collectProjectIDs([]byte(child.Raw), source, name == "project" || name == "projects", depth+1, found)
		return true
	})
}

func replaceProjectUUIDs(text string, rewrite func(string) (string, error)) (string, error) {
	var out strings.Builder
	offset := 0
	for _, at := range projectUUIDPattern.FindAllStringIndex(text, -1) {
		wire := text[at[0]:at[1]]
		var decoded strings.Builder
		for i := 0; i < len(wire); i++ {
			if wire[i] == '\\' {
				n, e := strconv.ParseUint(wire[i+2:i+6], 16, 8)
				if e != nil {
					return "", e
				}
				decoded.WriteByte(byte(n))
				i += 5
			} else {
				decoded.WriteByte(wire[i])
			}
		}
		id, parseErr := uuid.Parse(decoded.String())
		if parseErr != nil || id == uuid.Nil {
			continue
		}
		next, err := rewrite(id.String())
		if err != nil {
			return "", err
		}
		if next == "" {
			continue
		}
		out.WriteString(text[offset:at[0]])
		// Keep the occurrence's escape shape and length, including UUIDs in paths.
		j := 0
		for i := 0; i < len(wire); i++ {
			if wire[i] == '\\' {
				fmt.Fprintf(&out, `\u%04x`, next[j])
				i += 5
			} else {
				out.WriteByte(next[j])
			}
			j++
		}
		offset = at[1]
	}
	if offset == 0 {
		return text, nil
	}
	out.WriteString(text[offset:])
	return out.String(), nil
}

func (s *projectIdentityState) publish(ctx context.Context) {
	d := &projectIdentityDiagnostic{Version: "project-account-v1", AccountID: s.binding.AccountID, ScopeHash: s.binding.Scope}
	for _, change := range s.changes {
		copy := *change
		copy.Sources = append([]string(nil), change.Sources...)
		d.Changes = append(d.Changes, copy)
	}
	sort.Slice(d.Changes, func(i, j int) bool { return d.Changes[i].Original < d.Changes[j].Original })
	if len(d.Changes) > 0 {
		observer := UpstreamTransportObserver(ctx)
		if observer != nil && observer.attempt.accountID == s.binding.AccountID {
			observer.updateOutboundIdentity(func(identity *outboundIdentityDiagnostic) { identity.ProjectMapping = d })
		}
	}
}

func (s *projectIdentityState) change(original, alias, source string, restored bool) {
	entry := s.changes[original]
	if entry == nil {
		entry = &projectIdentityChange{Original: original, Outbound: alias}
		s.changes[original] = entry
	}
	if restored {
		entry.Restored++
	} else {
		entry.Replaced++
	}
	for _, existing := range entry.Sources {
		if existing == source {
			return
		}
	}
	if len(entry.Sources) < 64 {
		entry.Sources = append(entry.Sources, source)
	}
}

// Called after routing/restart expansion, before protocol metadata is stripped.
// Re-application (HTTP -> WS, retries) recognizes this attempt's own aliases.
func PrepareCodexProjectOutbound(ctx context.Context, account *auth.Account, body []byte, headers http.Header) (context.Context, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	db, binding := protocolIdentityBinding(ctx, account)
	found := make(map[string][]string)
	collectProjectIDs(body, "body", false, 0, found)
	for key, values := range headers {
		if !strings.EqualFold(key, "X-Codex-Turn-Metadata") && projectControlField(key) == "" {
			continue
		}
		for _, value := range values {
			if field := projectControlField(key); field != "" {
				b, _ := json.Marshal(map[string]string{field: value})
				collectProjectIDs(b, "headers", false, 0, found)
			} else {
				collectProjectIDs([]byte(value), "headers."+key, false, 0, found)
			}
		}
	}
	if db == nil || binding.Scope == "" || account == nil {
		// Legacy direct executor embeddings have no durable ownership scope.
		// Metadata-only IDs are still stripped; identified business IDs fail closed.
		if len(found) > 0 {
			for _, field := range projectBusinessPaths {
				_, err := rewriteProjectJSON([]byte(gjson.GetBytes(body, field).Raw), field, 0, func(text, _ string) (string, error) {
					return replaceProjectUUIDs(text, func(id string) (string, error) {
						if _, exists := found[id]; exists {
							return "", projectIdentityError()
						}
						return "", nil
					})
				})
				if err != nil {
					return ctx, nil, projectIdentityError()
				}
			}
		}
		return ctx, body, nil
	}
	s := projectIdentityFrom(ctx)
	if s == nil || s.binding.Scope != binding.Scope || s.binding.AccountID != binding.AccountID || s.binding.AccountHash != binding.AccountHash {
		s = &projectIdentityState{db: db, binding: binding, forward: make(map[string]string), reverse: make(map[string]string), changes: make(map[string]*projectIdentityChange), projected: make(map[string]bool)}
		ctx = context.WithValue(ctx, projectIdentityContextKey{}, s)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.control == nil {
		var err error
		s.control, err = readProjectControlSnapshot(body, headers)
		if err != nil {
			return ctx, nil, err
		}
	}
	ownerHasProjects, lookupErr := db.IsKnownCodexProjectID(ctx, binding.Scope, "registered-projects")
	if lookupErr != nil {
		return ctx, nil, projectIdentityError()
	}
	for id, sources := range found {
		if s.reverse[id] != "" {
			continue
		}
		pair, alreadyMapped, err := db.RestoreCodexProjectID(ctx, binding, id)
		if err != nil {
			return ctx, nil, projectIdentityError()
		}
		if alreadyMapped {
			s.forward[pair.Public], s.reverse[id] = id, pair.Public
			if s.changes[pair.Public] == nil {
				s.changes[pair.Public] = &projectIdentityChange{Original: pair.Public, Outbound: id, Sources: append([]string(nil), sources...)}
			}
			continue
		}
		if err := db.RegisterCodexProjectID(ctx, binding.Scope, id); err != nil {
			return ctx, nil, projectIdentityError()
		}
		pair, err = db.ResolveCodexProjectID(ctx, binding, id)
		if err != nil {
			return ctx, nil, projectIdentityError()
		}
		s.forward[id], s.reverse[pair.Upstream] = pair.Upstream, id
		if s.changes[id] == nil {
			s.changes[id] = &projectIdentityChange{Original: id, Outbound: pair.Upstream, Sources: append([]string(nil), sources...)}
		}
	}
	for _, field := range projectBusinessPaths {
		if !ownerHasProjects && len(found) == 0 {
			break
		}
		v := gjson.GetBytes(body, field)
		if !v.Exists() {
			continue
		}
		next, err := rewriteProjectJSON([]byte(v.Raw), "body."+field, 0, func(text, path string) (string, error) {
			return replaceProjectUUIDs(text, func(id string) (string, error) {
				if s.reverse[id] != "" {
					return "", nil
				}
				alias, ok := s.forward[id]
				if !ok {
					known, err := db.IsKnownCodexProjectID(ctx, binding.Scope, id)
					if err != nil {
						return "", err
					}
					if known {
						pair, err := db.ResolveCodexProjectID(ctx, binding, id)
						if err != nil {
							return "", err
						}
						alias = pair.Upstream
						s.reverse[alias] = id
					}
					s.forward[id] = alias
				}
				if alias != "" {
					s.change(id, alias, path, false)
				}
				return alias, nil
			})
		})
		if err != nil {
			return ctx, nil, projectIdentityError()
		}
		if !bytes.Equal(next, []byte(v.Raw)) {
			body, err = sjson.SetRawBytes(body, field, next)
			if err != nil {
				return ctx, nil, projectIdentityError()
			}
		}
	}
	var err error
	s.active, err = db.HasCodexProjectMappings(ctx, binding)
	if err != nil {
		return ctx, nil, projectIdentityError()
	}
	s.publish(ctx)
	return ctx, body, nil
}

func restoreProjectText(ctx context.Context, account *auth.Account, text, path string) (string, error) {
	s := projectIdentityFrom(ctx)
	if s == nil || !s.active || account == nil || s.binding.AccountID != account.ID() || s.binding.AccountHash != turnStateAccountHash(account) {
		return text, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out, err := replaceProjectUUIDs(text, func(id string) (string, error) {
		original, ok := s.reverse[id]
		if !ok {
			pair, found, err := s.db.RestoreCodexProjectID(ctx, s.binding, id)
			if err != nil {
				return "", err
			}
			if found {
				original = pair.Public
			}
			s.reverse[id] = original
		}
		if original != "" {
			s.change(original, id, path, true)
		}
		return original, nil
	})
	s.publish(ctx)
	if err != nil {
		return "", projectIdentityError()
	}
	return out, nil
}

func restoreProjectResponse(ctx context.Context, account *auth.Account, data []byte) ([]byte, error) {
	if s := projectIdentityFrom(ctx); s == nil || !s.active {
		return data, nil
	}
	return rewriteProjectJSON(data, "response", 0, func(text, path string) (string, error) { return restoreProjectText(ctx, account, text, path) })
}
