package proxy

import (
	"context"
	"net/http"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var projectControlHeaders = map[string]string{
	"project_id":   "X-Codex-Project-Id",
	"workspace_id": "X-Codex-Workspace-Id",
}

type projectControlSnapshot struct {
	values  map[string]string
	flat    map[string]string
	headers map[string]bool
	body    bool
}

func projectControlField(key string) string {
	switch privacyField(key) {
	case "projectid", "xcodexprojectid":
		return "project_id"
	case "workspaceid", "xcodexworkspaceid":
		return "workspace_id"
	}
	return ""
}

func projectControlUUID(value string) string {
	id, err := uuid.Parse(value)
	if err != nil || len(value) != 36 || id == uuid.Nil {
		return ""
	}
	return id.String()
}

// Only protocol carriers participate in this snapshot. Unknown control
// containers and unsupported top-level extensions are not reintroduced.
// Current frame metadata wins over compatibility projections/stale headers.
func readProjectControlSnapshot(body []byte, headers http.Header) (*projectControlSnapshot, error) {
	snapshot := &projectControlSnapshot{values: map[string]string{}, flat: map[string]string{}, headers: map[string]bool{}}
	flat := gjson.GetBytes(body, "client_metadata")
	snapshot.body = flat.IsObject()
	read := func(object gjson.Result, recordFlat bool) (map[string]string, error) {
		values := map[string]string{}
		var failure error
		object.ForEach(func(key, value gjson.Result) bool {
			field := projectControlField(key.String())
			if field == "" {
				return true
			}
			id := ""
			if value.Type == gjson.String {
				id = projectControlUUID(value.String())
			}
			if previous, exists := values[field]; exists && previous != id {
				failure = codexAccountIdentityError("项目元数据包含冲突的重复字段，请检查客户端请求。")
				return false
			}
			values[field] = id
			if recordFlat {
				snapshot.flat[key.String()] = field
			}
			return true
		})
		return values, failure
	}
	flatValues, err := read(flat, true)
	if err != nil {
		return nil, err
	}
	embedded := flat.Get("x-codex-turn-metadata")
	if !embedded.Exists() {
		embedded = flat.Get("x_codex_turn_metadata")
	}
	canonical, err := read(diagnosticMetadataObject(embedded), false)
	if err != nil {
		return nil, err
	}
	headerValues := map[string]string{}
	for key, values := range headers {
		field := projectControlField(key)
		if field != "" {
			snapshot.headers[field] = true
		}
		for _, raw := range values {
			current := map[string]string{}
			if strings.EqualFold(key, codexTurnMetadataHeader) {
				current, err = read(gjson.Parse(raw), false)
				if err != nil {
					return nil, err
				}
			} else if field != "" {
				current[field] = projectControlUUID(raw)
			}
			for name, id := range current {
				if previous, exists := headerValues[name]; exists && previous != id && !embedded.Exists() && flatValues[name] == "" {
					return nil, codexAccountIdentityError("项目请求头包含冲突的重复字段，请检查客户端请求。")
				}
				headerValues[name] = id
			}
		}
	}
	for field := range projectControlHeaders {
		id, exists := canonical[field]
		if !exists {
			id, exists = flatValues[field]
		}
		if !exists && !embedded.Exists() {
			id = headerValues[field]
		}
		if id != "" {
			snapshot.values[field] = id
		}
	}
	return snapshot, nil
}

// The legacy stripping boundaries remain defensive. Only this request's
// durable account-scoped mapping may put project fields back on the wire.
// Account custom headers never become a new source of project identity.
func finalizeProjectControlMetadata(ctx context.Context, body []byte, headers http.Header) ([]byte, http.Header) {
	s := projectIdentityFrom(ctx)
	if s == nil {
		return body, headers
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.control == nil {
		return body, headers
	}
	body = StripCodexProjectMetadata(body)
	StripCodexProjectMetadataHeaders(headers)
	if len(s.control.values) == 0 {
		return body, headers
	}
	metadata := diagnosticMetadataObject(gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata"))
	canonical := "{}"
	if metadata.IsObject() {
		canonical = metadata.Raw
	}
	header := headers.Get(codexTurnMetadataHeader)
	if !gjson.Parse(header).IsObject() {
		header = "{}"
	}
	flatNames := make([]string, 0, len(s.control.flat))
	for name := range s.control.flat {
		flatNames = append(flatNames, name)
	}
	sort.Strings(flatNames)
	// A pooled WS handshake cannot retain request-specific header-only IDs.
	// Put that snapshot on response.create before the handshake is bounded.
	writeBody := s.control.body || gjson.GetBytes(body, "type").String() == "response.create"
	for _, field := range []string{"project_id", "workspace_id"} {
		original := s.control.values[field]
		if original == "" {
			continue
		}
		alias := s.forward[original]
		if alias == "" && s.reverse[original] != "" {
			alias, original = original, s.reverse[original]
		}
		if alias == "" {
			continue
		}
		record := func(path string) {
			if !s.projected[path] {
				s.change(original, alias, path, false)
				s.projected[path] = true
			}
		}
		if writeBody {
			canonical, _ = sjson.Set(canonical, field, alias)
			body, _ = sjson.SetBytes(body, "client_metadata."+field, alias)
			record("body.client_metadata." + field)
			record("body.client_metadata.x-codex-turn-metadata." + field)
			for _, name := range flatNames {
				if s.control.flat[name] == field {
					path := "client_metadata." + strings.NewReplacer(`\`, `\\`, ".", `\.`).Replace(name)
					body, _ = sjson.SetBytes(body, path, alias)
					record("body.client_metadata." + name)
				}
			}
		}
		header, _ = sjson.Set(header, field, alias)
		record("headers." + codexTurnMetadataHeader + "." + field)
		if s.control.headers[field] {
			headers.Set(projectControlHeaders[field], alias)
			record("headers." + projectControlHeaders[field])
		}
	}
	if writeBody {
		// Official client_metadata is a string map at the transport boundary.
		body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata", canonical)
	}
	deleteHeaderCaseInsensitive(headers, codexTurnMetadataHeader)
	headers.Set(codexTurnMetadataHeader, header)
	s.publish(ctx)
	return body, headers
}

func validateProjectControlMetadata(body []byte, headers http.Header) error {
	flat := gjson.GetBytes(body, "client_metadata")
	canonical := diagnosticMetadataObject(flat.Get("x-codex-turn-metadata"))
	for field, header := range projectControlHeaders {
		expected := ""
		check := func(value string) bool {
			if value == "" {
				return true
			}
			if expected == "" {
				expected = value
			}
			return value == expected
		}
		valid := true
		for _, object := range []gjson.Result{flat, canonical, gjson.Parse(headers.Get(codexTurnMetadataHeader))} {
			object.ForEach(func(key, value gjson.Result) bool {
				if projectControlField(key.String()) == field {
					valid = value.Type == gjson.String && check(value.String())
				}
				return valid
			})
			if !valid {
				break
			}
		}
		if !valid || !check(headers.Get(header)) {
			return codexAccountIdentityError("出站项目字段不一致，已停止发送：" + field)
		}
	}
	return nil
}
