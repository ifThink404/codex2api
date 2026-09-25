package proxy

import (
	"context"
	"strconv"

	"github.com/codex2api/database"
)

type bpsRoundIdentityStore interface {
	bpsWordIdentityStore
	ResolveBPSRoundIdentity(context.Context, string, string, int) (database.BPSRoundIdentity, bool, error)
}

func resolveBPSRoundIdentity(ctx context.Context, scope *bpsFullConvergenceScope, turnKey, stepKey string, d *bpsWordIdentityDiagnostic) (*bpsWordIdentityDiagnostic, error) {
	store, ok := ctx.Value(codexIdentityClaimerContextKey{}).(bpsRoundIdentityStore)
	if !ok {
		return nil, codexAccountIdentityError("BPS 轮次收敛需要可用的持久化身份存储。")
	}
	step := codexIdentityDigest("bps-round-step-v1", turnKey, stepKey)
	assignment, reused, err := store.ResolveBPSRoundIdentity(ctx, scope.taskKey, step, scope.roundLimit)
	if err != nil {
		return nil, err
	}
	taskKey := codexIdentityDigest("bps-round-task-v1", scope.taskKey, strconv.FormatInt(assignment.Generation, 10))
	d.TaskID, err = store.ResolveCodexIdentityUUIDv7(ctx, taskKey, codexIdentityDigest("bps-round-task-entropy-v1", taskKey))
	if err != nil {
		return nil, err
	}
	// A new inference step, including a tool continuation, gets a separate
	// turn ID. The persisted step mapping keeps retries on that same ID.
	turnKey = codexIdentityDigest("bps-round-turn-v1", taskKey, step)
	d.TurnID, err = store.ResolveCodexIdentityUUIDv7(ctx, turnKey, codexIdentityDigest("bps-round-turn-entropy-v1", turnKey))
	if err != nil {
		return nil, err
	}
	d.AgentIteration = strconv.FormatInt(assignment.Iteration, 10)
	d.TaskScope, d.Persisted, d.ReusedStep = "upstream_account_rounds", true, reused
	d.TaskGeneration, d.RoundLimit = assignment.Generation, assignment.RoundLimit
	return d, nil
}
