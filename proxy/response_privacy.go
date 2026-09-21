package proxy

import (
	"bytes"
	"context"
	"errors"
	"regexp"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

var responseIDInError = regexp.MustCompile(`\bresp_[A-Za-z0-9_-]+`)

func maskResponseTurnState(ctx context.Context, account *auth.Account, value, carrier string) (string, error) {
	state := turnStateSessionFrom(ctx)
	if state == nil {
		return "", nil
	}
	// Header dictionaries and their enclosing response may both pass through
	// the shared mapper. Only aliases issued for this very request are idempotent.
	state.mu.Lock()
	generation := uint64(0)
	if epoch := outboundEpochFromContext(ctx); epoch != nil {
		generation = epoch.record.FailoverCount
	}
	for _, record := range state.issued {
		if record.Alias == value && account != nil && record.AccountID == account.ID() && record.Generation == generation && record.AccountHash == turnStateAccountHash(account) {
			state.mu.Unlock()
			return value, nil
		}
	}
	state.mu.Unlock()
	return state.issue(ctx, account, value, carrier)
}

// Mapping runs before classification; public error projection runs at the write
// boundary so retry and policy decisions still inspect the original message.
func maskResponsePayload(ctx context.Context, account *auth.Account, data []byte, jsonResponse bool) ([]byte, error) {
	trimmed := bytes.TrimSpace(data)
	if !jsonResponse && (len(trimmed) == 0 || bytes.Equal(trimmed, []byte("[DONE]"))) {
		return data, nil
	}
	parsed := gjson.ParseBytes(data)
	if !gjson.ValidBytes(data) || !parsed.IsObject() {
		return nil, errors.New("invalid upstream response envelope")
	}
	// Streaming deltas require cross-event buffering. Their caller restores
	// them without changing event order; complete JSON envelopes are restored here.
	if jsonResponse || !projectTextDelta(parsed.Get("type").String()) {
		var err error
		data, err = restoreProjectResponse(ctx, account, data)
		if err != nil {
			return nil, err
		}
		parsed = gjson.ParseBytes(data)
	}
	interesting := jsonResponse
	if !interesting && parsed.IsObject() {
		parsed.ForEach(func(key, value gjson.Result) bool {
			field := privacyField(key.String())
			// An object outside a business payload can contain protocol metadata.
			interesting = privateResponseField(key.String()) || isTurnStateField(key.String()) ||
				isTurnStateContainer(key.String()) || field == "responseid" || field == "previousresponseid" || field == "comparisonresponseid" || field == "parentresponseid" ||
				field == "error" || field == "response" ||
				((!responseBusinessField(key.String()) || field == "output") && (value.IsObject() || value.IsArray()))
			if field == "message" || field == "detail" {
				interesting = interesting || parsed.Get("type").String() == "error"
			}
			return !interesting
		})
	}
	if !interesting {
		return data, nil
	}
	return (responsePrivacyWalker{ctx: ctx, account: account}).rewrite(data, jsonResponse, false, parsed.Get("type").String() == "error", 0)
}
