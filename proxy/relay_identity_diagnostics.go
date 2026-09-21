package proxy

import (
	"context"
	"net/http"
	"sort"
	"strings"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

// Keep a bounded local comparison for the relay mapper, just as for native
// accounts. These values are never inserted into request headers or JSON.
func prepareRelayOutboundWithDiagnostic(ctx context.Context, account *auth.Account, body []byte, headers http.Header) (context.Context, []byte, http.Header, error) {
	originalBody, originalHeaders := body, headers.Clone()
	body, headers, err := prepareRelayOutboundPrivacy(ctx, account, body, headers)
	if err != nil || account == nil || !account.IsOpenAIResponsesAPI() {
		return ctx, body, headers, err
	}
	d := &codexAccountIdentityDiagnostic{Version: "relay-outbound-v1", Status: "unchanged"}
	changes := make(map[string]*codexAccountIdentityChange)
	add := func(field, before, after string) {
		if before == "" || after == "" || before == after || len(before) > 2048 || len(after) > 2048 {
			return
		}
		key := before + "\x00" + after
		change := changes[key]
		if change == nil {
			if len(changes) >= 32 {
				return
			}
			change = &codexAccountIdentityChange{Original: before, Outbound: after}
			changes[key] = change
		}
		change.Fields = append(change.Fields, field)
	}
	fields := append(append([]string{}, codexAccountIdentityFields...), "turn_id", "root_turn_id", "parent_turn_id", "window_id", "x-codex-window-id", "x-client-request-id", "installation_id", "x-codex-installation-id")
	for _, path := range []string{"client_metadata", "client_metadata.x-codex-turn-metadata"} {
		before := diagnosticMetadataObject(gjson.GetBytes(originalBody, path))
		after := diagnosticMetadataObject(gjson.GetBytes(body, path))
		for _, field := range fields {
			add(path+"."+field, before.Get(field).String(), after.Get(field).String())
		}
	}
	for _, name := range []string{codexSessionIDHeader, codexThreadIDHeader, codexParentThreadIDHeader, "X-Client-Request-Id", "X-Codex-Window-Id", "X-Codex-Installation-Id", "Idempotency-Key"} {
		add("prepared_headers."+name, originalHeaders.Get(name), headers.Get(name))
	}
	d.CachePartitioned = gjson.GetBytes(originalBody, "prompt_cache_key").String() != gjson.GetBytes(body, "prompt_cache_key").String()
	for _, change := range changes {
		sort.Strings(change.Fields)
		d.Changes = append(d.Changes, *change)
	}
	sort.Slice(d.Changes, func(i, j int) bool {
		return strings.Join(d.Changes[i].Fields, ",") < strings.Join(d.Changes[j].Fields, ",")
	})
	if len(d.Changes) > 0 || d.CachePartitioned {
		d.Status = "mapped"
	}
	return context.WithValue(ctx, codexAccountIdentityDiagnosticKey{}, d), body, headers, nil
}
