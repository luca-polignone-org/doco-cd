package stages

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/rollback"
)

// newFullTargetManager is newForkFailureManager with every field of the upstream failure target
// set (v0.129.0 keys DeploymentFailures by context, stack, repository, config source and config
// target), as a production run has them: a test that leaves them empty cannot tell the full
// target from the (repository, stack) key of docker.RecordDeploymentFailure.
func newFullTargetManager(t *testing.T) (*StageManager, string) {
	t.Helper()

	sm, data := newForkFailureManager(t, &deploy.RollbackConfig{Enabled: true})
	sm.DeployConfig.Context = "edge"
	sm.DeployConfig.Internal.ConfigTarget = "prod"
	sm.Repository.ConfigSourceUrl = "https://git.example.com/org/repo.git"

	t.Cleanup(func() { docker.DeploymentFailures.Clear(sm.deploymentFailureTarget()) })

	return sm, data
}

func fullTargetDiskFailure(t *testing.T, sm *StageManager) rollback.Failure {
	t.Helper()

	f, ok, err := rollback.StoreFor(sm.AppConfig.DataMountPath).GetFailure("edge/web")
	if err != nil || !ok {
		t.Fatalf("failure on disk: ok=%v err=%v", ok, err)
	}

	return f
}

// The rollback reads the failure that recordDeploymentFailure just recorded under the full target.
func TestHandleStageFailureRollbackFindsFullTarget(t *testing.T) {
	sm, data := newFullTargetManager(t)

	if _, ok := docker.GetDeploymentFailure(sm.Repository.Name, sm.DeployConfig.Name); ok {
		t.Fatal("a failure is recorded under the (repository, stack) key before the test")
	}

	// An unreadable last good: the rollback, when it runs, ends blocked/invalid.
	path := filepath.Join(data, "fork", "lastgood", "edge", "web.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	_ = sm.handleStageFailure(t.Context(), StageDeploy, sm.Log, errors.New("boom"))

	if f := fullTargetDiskFailure(t, sm); f.State != rollback.StateBlocked || f.Decision != "invalid" || f.CommitSHA != "c1" {
		t.Fatalf("failure %+v, want blocked/invalid: the rollback did not find the failure", f)
	}
}

// After a restart the disk record goes back into memory under the full target, with the
// failed_at that forkRetryRecreates compares.
func TestLastDeploymentFailureRestoresFullTarget(t *testing.T) {
	sm, _ := newFullTargetManager(t)

	sm.recordDeploymentFailure(StageDeploy, errors.New("x"))

	before, ok := docker.DeploymentFailures.Get(sm.deploymentFailureTarget())
	if !ok {
		t.Fatal("recordDeploymentFailure() recorded nothing under the full target")
	}

	if disk := fullTargetDiskFailure(t, sm); !disk.FailedAt.Equal(before.FailedAt) || disk.State != rollback.StateFailed {
		t.Fatalf("disk failure %+v, memory failed_at %v", disk, before.FailedAt)
	}

	docker.DeploymentFailures.Clear(sm.deploymentFailureTarget()) // restart

	got, ok := sm.lastDeploymentFailure()
	if !ok || got.CommitSHA != "c1" || !got.FailedAt.Equal(before.FailedAt) {
		t.Fatalf("lastDeploymentFailure() = %+v, %v", got, ok)
	}

	if mem, ok := docker.DeploymentFailures.Get(sm.deploymentFailureTarget()); !ok || !mem.FailedAt.Equal(before.FailedAt) {
		t.Fatalf("memory after restore = %+v, %v", mem, ok)
	}

	// A failure already in memory wins over the disk record.
	if f := docker.DeploymentFailures.RestoreForkFailure(sm.deploymentFailureTarget(), docker.DeploymentFailure{CommitSHA: "other"}); f.CommitSHA != "c1" {
		t.Fatalf("RestoreForkFailure() replaced the failure in memory: %+v", f)
	}
}

// Memory truncates the error to 2000 runes before the fork sees it: the disk record must redact
// the full error first, or a secret across the cut would survive in part.
func TestRecordDeploymentFailureRedactsBeforeTruncation(t *testing.T) {
	sm, _ := newFullTargetManager(t)

	secret := "s3cr3t-value-that-is-long-enough"
	sm.DeployConfig.Internal.Environment = map[string]string{"TOKEN": secret}

	sm.recordDeploymentFailure(StageDeploy, errors.New(strings.Repeat("e", 1990)+secret))

	mem, _ := docker.DeploymentFailures.Get(sm.deploymentFailureTarget())
	if !strings.Contains(mem.Error, secret[:10]) {
		t.Fatal("test setup: the memory record should keep the head of the secret across the cut")
	}

	if disk := fullTargetDiskFailure(t, sm); strings.Contains(disk.Error, secret[:6]) {
		t.Fatalf("disk failure keeps part of the secret: %q", disk.Error[len(disk.Error)-40:])
	}
}

// An interruption that keeps the recorded failure leaves the disk record alone, including its
// rollback state.
func TestRecordDeploymentFailureInterruptedKeepsDisk(t *testing.T) {
	sm, _ := newFullTargetManager(t)

	sm.recordDeploymentFailure(StageDeploy, errors.New("x"))

	st := rollback.StoreFor(sm.AppConfig.DataMountPath)
	f := fullTargetDiskFailure(t, sm)
	f.State, f.RolledBackTo = rollback.StateRolledBack, "good"

	if err := st.PutFailure(f); err != nil {
		t.Fatal(err)
	}

	sm.Repository.Revision = "c2"
	sm.recordDeploymentFailure(StageDeploy, context.Canceled)

	if got := fullTargetDiskFailure(t, sm); got.State != rollback.StateRolledBack || got.CommitSHA != "c1" || got.RolledBackTo != "good" {
		t.Fatalf("disk failure %+v, want the rolled_back record of c1 unchanged", got)
	}
}

// recordTargetFailure puts failure in memory under the full upstream target of sm, replacing any
// failure there: since v0.129.0 docker.RecordDeploymentFailure keys only (repository, stack),
// which the rollback no longer reads when the run has a config source or a context.
func recordTargetFailure(t *testing.T, sm *StageManager, failure docker.DeploymentFailure) {
	t.Helper()

	target := sm.deploymentFailureTarget()
	docker.DeploymentFailures.Clear(target)
	docker.DeploymentFailures.RestoreForkFailure(target, failure)

	t.Cleanup(func() { docker.DeploymentFailures.Clear(target) })
}
