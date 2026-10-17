package rollback

import (
	"context"
	"fmt"
	"time"

	"github.com/kimdre/doco-cd/internal/hooks"
)

// Decider asks the pre_rollback hook for a veto. *hooks.PreRollbackClient implements it, a nil
// one included (it answers unconfigured).
type Decider interface {
	Decide(ctx context.Context, req hooks.PreRollbackRequest, timeout time.Duration) hooks.Decision
}

// Executor redeploys the last good: it republishes the artifact of its commit, renders it with
// the saved deploy config, pins the images, applies the compose policy in force and force
// recreates the whole project.
type Executor interface {
	Deploy(ctx context.Context, lg LastGood) error
}

// Outcome is the result of Run. Err is the reason of a blocked (unreadable last good) or
// rollback_failed outcome, for the log; PersistErr is the error of the final store write.
type Outcome struct {
	State      State
	Decision   hooks.Decision
	ToCommit   string
	Err        error
	PersistErr error
}

// Run decides and runs the rollback after a failed deploy. The failure is already on disk with
// state failed (recordDeploymentFailure writes it), so a crash halfway still leaves the commit
// marked. Any pre_rollback outcome other than proceed blocks, and an unreadable last good blocks
// without asking: never a blind rollback.
func Run(ctx context.Context, store *Store, decider Decider, exec Executor, f Failure, preTimeout, timeout time.Duration) Outcome {
	lg, ok, err := store.GetLastGood(f.Object)
	if err != nil {
		return finish(store, f, StateBlocked, hooks.DecisionInvalid, "", err)
	}

	if !ok {
		return finish(store, f, StateFailed, "", "", nil)
	}

	decision := decider.Decide(ctx, hooks.PreRollbackRequest{
		Object: f.Object, FailedCommit: f.CommitSHA, LastGoodCommit: lg.CommitSHA,
		Stage: f.Stage, Error: f.Error,
	}, preTimeout)
	if decision != hooks.DecisionProceed {
		return finish(store, f, StateBlocked, decision, "", nil)
	}

	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err = exec.Deploy(rctx, lg); err != nil {
		return finish(store, f, StateRollbackFailed, decision, lg.CommitSHA, fmt.Errorf("rollback to %s: %w", lg.CommitSHA, err))
	}

	return finish(store, f, StateRolledBack, decision, lg.CommitSHA, nil)
}

func finish(store *Store, f Failure, state State, decision hooks.Decision, to string, cause error) Outcome {
	f.State, f.Decision = state, string(decision)

	return Outcome{State: state, Decision: decision, ToCommit: to, Err: cause, PersistErr: store.PutFailure(f)}
}
