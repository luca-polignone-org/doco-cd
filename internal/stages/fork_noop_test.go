package stages

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/hooks"
)

// TestForkHooksAreNoOpsWithoutKeys checks the non-regression guarantee of the fork at the level
// of its hooks in the stage manager: with no fork key (no rollback in the deploy config, no
// compose policy, no RBAC enforcer, no outbox, no pre_rollback client) every fork hook returns
// the upstream answer, emits nothing and writes nothing under <data>/fork/. A full deploy up to
// RunStages needs Docker, so the hooks are called one by one instead. It needs no Docker.
func TestForkHooksAreNoOpsWithoutKeys(t *testing.T) {
	// The fork registries and the in-memory failure map are process globals: no t.Parallel().
	prevPolicy, prevRBAC, prevOutbox, prevPreRollback := composePolicy.Load(), rbacEnforcer.Load(), outcomeOutbox.Load(), preRollbackClient.Load()
	prevGate := docker.ForkJobGate

	SetComposePolicy(nil)
	SetRBAC(nil)
	SetOutbox(nil)
	SetPreRollback(nil)

	t.Cleanup(func() {
		composePolicy.Store(prevPolicy)
		rbacEnforcer.Store(prevRBAC)
		outcomeOutbox.Store(prevOutbox)
		preRollbackClient.Store(prevPreRollback)

		docker.ForkJobGate = prevGate
	})

	if docker.ForkJobGate != nil {
		t.Fatal("SetComposePolicy(nil) must leave docker.ForkJobGate unset")
	}

	sm := newTestStageManager(t)
	data := t.TempDir()
	sm.AppConfig.DataMountPath = data
	sm.Repository.Name = "org/fork-noop"
	sm.Repository.Revision = "c1"
	sm.DeployConfig.Name = "fork-noop"
	sm.DeployConfig.Rollback = nil

	t.Cleanup(func() { docker.ClearDeploymentFailure("org/fork-noop", "fork-noop") })

	ctx := context.Background()
	cause := errors.New("deploy failed")

	if sm.forkStore() != nil {
		t.Fatal("forkStore() must be nil without the rollback key")
	}

	sm.recordDeploymentFailure(StageDeploy, cause)

	if _, ok := docker.GetDeploymentFailure("org/fork-noop", "fork-noop"); !ok {
		t.Fatal("recordDeploymentFailure() must still record the upstream in-memory failure")
	}

	if sm.runForkRollback(ctx, StageDeploy, cause) {
		t.Fatal("runForkRollback() must not handle a failure without the rollback key")
	}

	if images := sm.recordLastGood(ctx); images != nil {
		t.Fatalf("recordLastGood() = %v, want nil", images)
	}

	if err := sm.checkForkPolicies(ctx); err != nil {
		t.Fatalf("checkForkPolicies() error = %v, want nil", err)
	}

	if err := forkJobGate("", "fork-noop"); err != nil {
		t.Fatalf("forkJobGate() error = %v, want nil", err)
	}

	if sm.forkPoliciesActive(ctx) {
		t.Fatal("forkPoliciesActive() must be false without fork keys")
	}

	if err := sm.forkPolicyGate(ctx); err != nil {
		t.Fatalf("forkPolicyGate() error = %v, want nil", err)
	}

	if err := sm.checkTargetSync(ctx, sm.DeployConfig.Object()); err != nil {
		t.Fatalf("checkTargetSync() error = %v, want nil", err)
	}

	if sm.rollbackPoisoned(ctx, "c1", sm.Log) {
		t.Fatal("rollbackPoisoned() must be false without the rollback key")
	}

	if sm.emitOutcome(ctx, hooks.OutcomeDeployed) {
		t.Fatal("emitOutcome() must not enqueue without an outbox")
	}

	sm.emitSuccessOutcome(ctx, nil)
	sm.emitFailureOutcome(ctx, StageDeploy, cause)

	if EmitOutcome(sm.Log, hooks.NewEvent(sm.DeployConfig.Object())) {
		t.Fatal("EmitOutcome() must not enqueue without an outbox")
	}

	sm.clearDeploymentFailure()

	if _, ok := sm.lastDeploymentFailure(); ok {
		t.Fatal("lastDeploymentFailure() must find nothing after clearDeploymentFailure()")
	}

	if _, err := os.Stat(filepath.Join(data, "fork")); !os.IsNotExist(err) {
		t.Fatalf("expected no %s, got err=%v", filepath.Join(data, "fork"), err)
	}
}
