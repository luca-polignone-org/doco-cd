package rollback

import (
	"context"
	"errors"
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
	// Supports reports, before pre_rollback is asked, why lg cannot be rolled back at all (e.g.
	// an unsupported source): asking for a veto on a rollback that cannot happen would record a
	// misleading proceed.
	Supports(lg LastGood) error
	Deploy(ctx context.Context, lg LastGood) error
}

// Outcome is the result of Run. Err is the reason of a blocked (unreadable last good) or
// rollback_failed outcome, for the log; PersistErr is the error of the final store write.
// LastGood is the last good Run read, nil when there is none or it could not be read: the
// outcome event reports its commit, and its images once it runs again.
type Outcome struct {
	State      State
	Decision   hooks.Decision
	ToCommit   string
	Err        error
	PersistErr error
	LastGood   *LastGood
}

// Run decides and runs the rollback after a failed deploy. The failure is already on disk with
// state failed (recordDeploymentFailure writes it), so a crash halfway still leaves the commit
// marked. Any pre_rollback outcome other than proceed blocks, and an unreadable last good blocks
// without asking: never a blind rollback.
//
// A job cancelled by shutdown (context.Canceled) during pre_rollback or the rollback keeps the
// failure failed: a sticky blocked or rollback_failed would outlive the restart, while a hard
// crash at the same point leaves failed too, and the next run retries.
func Run(ctx context.Context, store *Store, decider Decider, exec Executor, f Failure, preTimeout, timeout time.Duration) Outcome {
	lg, ok, err := store.GetLastGood(f.Object)
	if err != nil {
		return finish(store, f, StateBlocked, hooks.DecisionInvalid, "", err)
	}

	if !ok {
		return finish(store, f, StateFailed, "", "", nil)
	}

	out := runFrom(ctx, store, decider, exec, f, lg, preTimeout, timeout)
	out.LastGood = &lg

	return out
}

// runFrom is the part of Run that has a last good to roll back to.
func runFrom(ctx context.Context, store *Store, decider Decider, exec Executor, f Failure, lg LastGood, preTimeout, timeout time.Duration) Outcome {
	if err := exec.Supports(lg); err != nil {
		return finish(store, f, StateRollbackFailed, "", "", err)
	}

	decision := decider.Decide(ctx, hooks.PreRollbackRequest{
		Object: f.Object, FailedCommit: f.CommitSHA, LastGoodCommit: lg.CommitSHA,
		Stage: f.Stage, Error: f.Error,
	}, preTimeout)
	if errors.Is(ctx.Err(), context.Canceled) {
		return finish(store, f, StateFailed, "", "", ctx.Err())
	}

	if decision != hooks.DecisionProceed {
		return finish(store, f, StateBlocked, decision, "", nil)
	}

	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := exec.Deploy(rctx, lg); err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return finish(store, f, StateFailed, "", "", ctx.Err())
		}

		return finish(store, f, StateRollbackFailed, decision, lg.CommitSHA, fmt.Errorf("rollback to %s: %w", lg.CommitSHA, err))
	}

	return finish(store, f, StateRolledBack, decision, lg.CommitSHA, nil)
}

func finish(store *Store, f Failure, state State, decision hooks.Decision, to string, cause error) Outcome {
	f.State, f.Decision = state, string(decision)

	return Outcome{State: state, Decision: decision, ToCommit: to, Err: cause, PersistErr: store.PutFailure(f)}
}
