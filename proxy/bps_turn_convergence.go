package proxy

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
)

type bpsTurnIdentityStore interface {
	bpsWordIdentityStore
	ResolveBPSTurnTaskIdentity(context.Context, string, string, int) (database.BPSTurnTaskIdentity, bool, error)
	TouchBPSTurnTaskIdentity(context.Context, string, int64) (database.BPSRoundBatchActivity, error)
}

func resolveBPSTurnIdentity(ctx context.Context, scope *bpsFullConvergenceScope, model, effort, userTurnKey, stepKey string, d *bpsWordIdentityDiagnostic) (*bpsWordIdentityDiagnostic, error) {
	store, ok := ctx.Value(codexIdentityClaimerContextKey{}).(bpsTurnIdentityStore)
	if !ok {
		return nil, codexAccountIdentityError("BPS turn_id 轮次需要可用的持久化身份存储。")
	}
	model = strings.TrimSpace(model)
	partitionKey := codexIdentityDigest("bps-turn-account-model-effort-v1", scope.taskKey, model, effort)
	step := codexIdentityDigest("bps-turn-step-v1", userTurnKey, stepKey)
	assignment, _, err := store.ResolveBPSTurnTaskIdentity(ctx, partitionKey, step, scope.taskLifetimeHours)
	if err != nil {
		return nil, err
	}
	taskKey := codexIdentityDigest("bps-turn-task-v1", partitionKey, strconv.FormatInt(assignment.Generation, 10))
	d.TaskID, err = store.ResolveCodexIdentityUUIDv7(ctx, taskKey, codexIdentityDigest("bps-turn-task-entropy-v1", taskKey))
	if err != nil {
		return nil, err
	}
	// The scoped user turn remains stable across tool continuations. A new
	// user turn or timed task generation starts its own iteration counter.
	turnKey := codexIdentityDigest("bps-turn-user-v1", taskKey, userTurnKey)
	d.TurnID, err = store.ResolveCodexIdentityUUIDv7(ctx, turnKey, codexIdentityDigest("bps-turn-user-entropy-v1", turnKey))
	if err != nil {
		return nil, err
	}
	iteration, reused, err := store.ResolveBPSWordIteration(ctx, turnKey, stepKey)
	if err != nil {
		return nil, err
	}
	d.AgentIteration = strconv.FormatInt(iteration, 10)
	d.TaskScope, d.Persisted, d.ReusedStep = "upstream_account_model_effort_timed_turns", true, reused
	d.TaskModel, d.TaskReasoningEffort = model, effort
	d.TaskGeneration, d.TaskLifetimeHours = assignment.Generation, assignment.LifetimeHours
	d.roundPartitionKey = partitionKey
	return d, nil
}

func touchBPSTurnIdentity(ctx context.Context, d *bpsWordIdentityDiagnostic) error {
	if d == nil {
		return nil
	}
	store, ok := ctx.Value(codexIdentityClaimerContextKey{}).(bpsTurnIdentityStore)
	if !ok || d.roundPartitionKey == "" {
		return codexAccountIdentityError("BPS turn_id 轮次需要可用的持久化身份存储。")
	}
	activity, err := store.TouchBPSTurnTaskIdentity(ctx, d.roundPartitionKey, d.TaskGeneration)
	if err != nil {
		return err
	}
	d.TaskStartedAtMS, d.TaskLastSentAtMS = activity.StartedAtMS, activity.LastSentAtMS
	d.TaskExpiresAtMS = activity.StartedAtMS + (time.Duration(d.TaskLifetimeHours) * time.Hour).Milliseconds()
	return nil
}
