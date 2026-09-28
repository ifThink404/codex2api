package proxy

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

type bpsRoundIdentityStore interface {
	bpsWordIdentityStore
	ResolveBPSRoundIdentity(context.Context, string, string, int, int) (database.BPSRoundIdentity, bool, error)
	TouchBPSRoundIdentity(context.Context, string, int64) (database.BPSRoundBatchActivity, error)
}

// Use the effective BPS level: an omitted effort defaults to low, max maps to
// xhigh, and the last protocol configuration update overrides the baseline.
// Tool data is opaque. Compact requests use their supplied/default partition
// without adding a reasoning parameter to the compact wire format.
func bpsRoundReasoningEffort(body []byte) string {
	effort := extractReasoningEffort(body)
	for _, item := range gjson.GetBytes(body, "input").Array() {
		if item.Get("type").String() == "configuration_update" {
			if value := item.Get("reasoning.effort"); value.Type == gjson.String && strings.TrimSpace(value.String()) != "" {
				effort = value.String()
			}
		}
	}
	effort = strings.ToLower(strings.TrimSpace(effort))
	switch effort {
	case "":
		return defaultCodexReasoningEffort
	case "max":
		return "xhigh"
	default:
		return effort
	}
}

func resolveBPSRoundIdentity(ctx context.Context, scope *bpsFullConvergenceScope, model, effort, turnKey, stepKey string, d *bpsWordIdentityDiagnostic) (*bpsWordIdentityDiagnostic, error) {
	store, ok := ctx.Value(bpsIdentityStoreKey{}).(bpsRoundIdentityStore)
	if !ok {
		return nil, codexAccountIdentityError("BPS 轮次收敛需要可用的持久化身份存储。")
	}
	step := codexIdentityDigest("bps-round-step-v1", turnKey, stepKey)
	// Partition both the durable counter and task UUID by the model and effort
	// actually used upstream. Aliases of the same combination share a counter.
	model = strings.TrimSpace(model)
	partitionKey := codexIdentityDigest("bps-round-account-model-effort-v1", scope.taskKey, model, effort)
	assignment, reused, err := store.ResolveBPSRoundIdentity(ctx, partitionKey, step, scope.roundLimit, scope.taskLifetimeHours)
	if err != nil {
		return nil, err
	}
	taskKey := codexIdentityDigest("bps-round-task-v1", partitionKey, strconv.FormatInt(assignment.Generation, 10))
	d.TaskID, err = store.ResolveCodexIdentityUUIDv7(ctx, taskKey, codexIdentityDigest("bps-round-task-entropy-v1", taskKey))
	if err != nil {
		return nil, err
	}
	// Keep a fixed turn while the task and account residency are unchanged.
	// A failover (including a return to this account) starts a new turn without
	// rotating the destination account's task or resetting its batch counter.
	turnKey = codexIdentityDigest("bps-round-fixed-turn-v1", taskKey)
	if scope.turnEpoch != "" {
		turnKey = codexIdentityDigest("bps-round-segment-turn-v1", taskKey, scope.turnEpoch)
	}
	d.TurnID, err = store.ResolveCodexIdentityUUIDv7(ctx, turnKey, codexIdentityDigest("bps-round-turn-entropy-v1", turnKey))
	if err != nil {
		return nil, err
	}
	d.AgentIteration = strconv.FormatInt(assignment.Iteration, 10)
	d.TaskScope, d.Persisted, d.ReusedStep = "upstream_account_model_effort_rounds", true, reused
	d.TaskModel, d.TaskReasoningEffort = model, effort
	d.TaskGeneration, d.RoundLimit = assignment.Generation, assignment.RoundLimit
	d.TaskLifetimeHours = assignment.LifetimeHours
	d.roundPartitionKey = partitionKey
	return d, nil
}

func touchBPSRoundIdentity(ctx context.Context, d *bpsWordIdentityDiagnostic) error {
	if d == nil {
		return nil
	}
	store, ok := ctx.Value(bpsIdentityStoreKey{}).(bpsRoundIdentityStore)
	if !ok || d.roundPartitionKey == "" {
		return codexAccountIdentityError("BPS 轮次收敛需要可用的持久化身份存储。")
	}
	activity, err := store.TouchBPSRoundIdentity(ctx, d.roundPartitionKey, d.TaskGeneration)
	if err != nil {
		return err
	}
	d.TaskStartedAtMS = activity.StartedAtMS
	d.TaskLastSentAtMS = activity.LastSentAtMS
	d.TaskExpiresAtMS = activity.StartedAtMS + (time.Duration(d.TaskLifetimeHours) * time.Hour).Milliseconds()
	return nil
}
