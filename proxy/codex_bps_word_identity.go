package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

type bpsWordIdentityStore interface {
	ResolveCodexIdentityUUIDv7(context.Context, string, string) (string, error)
	ResolveBPSWordIteration(context.Context, string, string) (int64, bool, error)
}

type bpsWordAccountScopeKey struct{}

// These are protocol identifiers, not secrets. Keep UUIDs and legacy task_/turn_
// IDs inspectable while rejecting arbitrary content masquerading as metadata.
func bpsWordDiagnosticIdentifier(value string) string {
	if len(value) == 0 || len(value) > 128 {
		return "[invalid_identifier]"
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == ':') {
			return "[invalid_identifier]"
		}
	}
	return strings.Clone(value)
}

type bpsWordIdentityDiagnostic struct {
	TaskID              string `json:"task_id"`
	TaskScope           string `json:"task_scope,omitempty"`
	TaskModel           string `json:"task_model,omitempty"`
	TaskReasoningEffort string `json:"task_reasoning_effort,omitempty"`
	TurnID              string `json:"turn_id"`
	AgentIteration      string `json:"agent_iteration"`
	TurnSource          string `json:"turn_source"`
	Persisted           bool   `json:"persisted"`
	ReusedStep          bool   `json:"reused_step"`
	Generation          uint64 `json:"generation,omitempty"`
	TaskGeneration      int64  `json:"task_generation,omitempty"`
	RoundLimit          int    `json:"round_limit,omitempty"`
	TaskStartedAtMS     int64  `json:"task_started_at_unix_ms,omitempty"`
	TaskLastSentAtMS    int64  `json:"task_last_sent_at_unix_ms,omitempty"`
	TaskExpiresAtMS     int64  `json:"task_expires_at_unix_ms,omitempty"`
	roundPartitionKey   string
}

// Canonicalize JSON without converting integers through float64. Keys and
// formatting in tool result objects must not turn retries into new iterations.
func bpsWordCanonical(value gjson.Result) string {
	if !value.Exists() {
		return ""
	}
	var decoded any
	decoder := json.NewDecoder(strings.NewReader(value.Raw))
	decoder.UseNumber()
	if decoder.Decode(&decoded) != nil {
		return value.Raw
	}
	encoded, _ := json.Marshal(decoded)
	return string(encoded)
}

// The latest user boundary partitions a turn when the client has no turn ID.
// A batch of parallel tool results is one step, regardless of result order.
func bpsWordInputIdentity(body []byte) (anchor, step string, iteration int) {
	input := gjson.GetBytes(body, "input")
	if input.Type == gjson.String {
		return codexIdentityDigest("word-user", input.String()), "initial", 1
	}
	items := input.Array()
	lastUser := -1
	for i, item := range items {
		if item.Get("role").String() == "user" {
			lastUser = i
		}
	}
	prefix := make([]string, 0, lastUser+1)
	for i := 0; i <= lastUser; i++ {
		// Runtime instructions/tools can change between HTTP attempts.
		role := items[i].Get("role").String()
		if role != "developer" && role != "system" && items[i].Get("type").String() != "additional_tools" {
			prefix = append(prefix, bpsWordCanonical(items[i]))
		}
	}
	if lastUser >= 0 {
		anchor = codexIdentityDigest(append([]string{"word-user-boundary"}, prefix...)...)
	}
	iteration = 1
	var batch []string
	flush := func() {
		if len(batch) == 0 {
			return
		}
		sort.Strings(batch)
		step = codexIdentityDigest(append([]string{"word-tool-batch"}, batch...)...)
		iteration++
		batch = nil
	}
	for _, item := range items[lastUser+1:] {
		switch item.Get("type").String() {
		case "function_call_output", "custom_tool_call_output":
			// Transport/history annotations are not part of the tool result.
			batch = append(batch, codexIdentityDigest(item.Get("type").String(), item.Get("call_id").String(), bpsWordCanonical(item.Get("output"))))
		case "function_call", "custom_tool_call":
			flush()
		}
	}
	flush()
	if step == "" {
		step = "initial"
	}
	return
}

func resolveBPSWordIdentity(ctx context.Context, body []byte, headers http.Header, cacheKey, model string, compact bool) (*bpsWordIdentityDiagnostic, error) {
	metadata := CodexRequestMetadataHeaders(headers, body)
	turnSeed := strings.TrimSpace(gjson.Get(metadata.Get(codexTurnMetadataHeader), "turn_id").String())
	if turnSeed == "" {
		turnSeed = strings.TrimSpace(gjson.GetBytes(body, "client_metadata.turn_id").String())
	}
	anchor, step, historyIteration := bpsWordInputIdentity(body)
	d := &bpsWordIdentityDiagnostic{TurnSource: "client_turn"}
	if turnSeed == "" {
		turnSeed, d.TurnSource = anchor, "user_boundary"
		if turnSeed == "" {
			// A truncated tool continuation without any turn identity is only a
			// heuristic. Keep it local to this task and expose that in diagnostics.
			turnSeed, d.TurnSource = "unidentified", "missing_user_boundary"
		}
	}
	epoch := outboundEpochFromContext(ctx)
	if epoch != nil {
		d.Generation = epoch.record.FailoverCount
	}
	// cacheKey has already been scoped by caller, account, profile and epoch.
	taskKey := codexIdentityDigest("bps-word-task-v2", cacheKey, epoch.identityKey())
	if accountScope, ok := ctx.Value(bpsWordAccountScopeKey{}).(string); ok {
		root := gjson.Get(metadata.Get(codexTurnMetadataHeader), "session_id").String()
		if root == "" {
			root = metadata.Get(codexSessionIDHeader)
		}
		if root != "" {
			// Native context windows/cache hints can rotate within one task.
			taskKey = codexIdentityDigest("bps-word-root-task-v2", accountScope, root, epoch.identityKey())
		}
	}
	turnKey := codexIdentityDigest("bps-word-turn-v2", taskKey, turnSeed)
	if full := bpsFullConvergenceFrom(ctx); full != nil {
		taskKey = full.taskKey
		turnKey = codexIdentityDigest("bps-full-turn-v1", full.turnScope, turnSeed)
		d.TaskScope = "upstream_account"
	}
	stepKey := codexIdentityDigest("bps-word-step-v2", step)
	if full := bpsFullConvergenceFrom(ctx); full != nil && full.roundLimit > 0 {
		stepKey = codexIdentityDigest("bps-round-operation-v1", stepKey, strconv.FormatBool(compact))
		return resolveBPSRoundIdentity(ctx, full, model, bpsRoundReasoningEffort(body), turnKey, stepKey, d)
	}
	store, ok := ctx.Value(codexIdentityClaimerContextKey{}).(bpsWordIdentityStore)
	if !ok {
		// Standalone projections without a database retain deterministic identity;
		// the live gateway supplies the durable store and uses creation timestamps.
		d.TaskID = DeriveStableSessionUUIDv7(taskKey)
		d.TurnID = DeriveStableSessionUUIDv7(turnKey)
		d.AgentIteration = strconv.Itoa(historyIteration)
		if d.Generation > 0 {
			d.AgentIteration = "1"
		}
		return d, nil
	}
	d.Persisted = true
	var err error
	d.TaskID, err = store.ResolveCodexIdentityUUIDv7(ctx, taskKey, codexIdentityDigest("bps-word-task-entropy-v2", taskKey))
	if err != nil {
		return nil, err
	}
	d.TurnID, err = store.ResolveCodexIdentityUUIDv7(ctx, turnKey, codexIdentityDigest("bps-word-turn-entropy-v2", turnKey))
	if err != nil {
		return nil, err
	}
	iteration, reused, err := store.ResolveBPSWordIteration(ctx, turnKey, stepKey)
	if err != nil {
		return nil, err
	}
	d.AgentIteration, d.ReusedStep = strconv.FormatInt(iteration, 10), reused
	return d, nil
}
