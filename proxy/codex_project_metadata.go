package proxy

import (
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func stripCodexProjectIdentifiers(raw string) (string, bool) {
	metadata := gjson.Parse(raw)
	if !metadata.IsObject() {
		return raw, false
	}
	changed := false
	metadata.ForEach(func(key, value gjson.Result) bool {
		if projectControlField(key.String()) != "" {
			if updated, err := sjson.Delete(raw, key.String()); err == nil && updated != raw {
				raw = updated
				changed = true
			}
		}
		return true
	})
	return raw, changed
}

func StripCodexProjectMetadata(body []byte) []byte {
	metadata := gjson.GetBytes(body, "client_metadata")
	if !metadata.IsObject() {
		return body
	}
	raw, changed := stripCodexProjectIdentifiers(metadata.Raw)
	for _, field := range []string{"x-codex-turn-metadata", "x_codex_turn_metadata"} {
		carrier := gjson.Get(raw, field)
		if carrier.Type != gjson.String && !carrier.IsObject() {
			continue
		}
		cleaned, modified := stripCodexProjectIdentifiers(carrier.String())
		if !modified {
			continue
		}
		var updated string
		var err error
		if carrier.IsObject() {
			updated, err = sjson.SetRaw(raw, field, cleaned)
		} else {
			updated, err = sjson.Set(raw, field, cleaned)
		}
		if err == nil {
			raw = updated
			changed = true
		}
	}
	if !changed {
		return body
	}
	if updated, err := sjson.SetRawBytes(body, "client_metadata", []byte(raw)); err == nil {
		return updated
	}
	return body
}

func StripCodexProjectMetadataHeaders(headers http.Header) {
	for name, values := range headers {
		switch {
		case projectControlField(name) != "":
			delete(headers, name)
		case strings.EqualFold(name, codexTurnMetadataHeader):
			copied := false
			for index, value := range values {
				if cleaned, changed := stripCodexProjectIdentifiers(value); changed {
					if !copied {
						values = append([]string(nil), values...)
						headers[name] = values
						copied = true
					}
					values[index] = cleaned
				}
			}
		}
	}
}
