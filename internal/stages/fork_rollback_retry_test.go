package stages

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/rollback"
)

// TCK-644: the rollback deploys the last good with the image references of its own compose file
// when they still name the images that ran, so its containers match a regular deploy of that
// content.
func TestRollbackExecutorKeepsReferencesOfLastGood(t *testing.T) {
	r := newRollbackRepo(t)
	sm := newRollbackManager(t, r, &lastGoodClient{refs: map[string]string{"alpine:3.22": "sha256:app", "busybox:1": "sha256:extra"}})
	got := stubDeployLastGood(t, nil)

	if err := (stageExecutor{s: sm, log: sm.Log}).Deploy(t.Context(), rollbackLastGood(r, "good", rollbackConfig)); err != nil {
		t.Fatal(err)
	}

	app, extra := (*got)[0].Project.Services["app"], (*got)[0].Project.Services["extra"]
	if app.Image != "alpine:3.22" || app.PullPolicy != "never" || app.Build != nil {
		t.Fatalf("app %+v, want alpine:3.22 with pull_policy never and no build", app)
	}

	if extra.Image != "busybox:1" || extra.PullPolicy != "never" {
		t.Fatalf("extra %+v, want busybox:1 with pull_policy never", extra)
	}
}

// TCK-644: after a successful rollback a new commit resets the record to failed, and its retry
// does not force recreate the containers of the rollback. A blocked failure still does.
func TestRetryAfterRolledBackDoesNotForceRecreate(t *testing.T) {
	r := newRollbackRepo(t)
	stageLog := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("rolled_back", func(t *testing.T) {
		sm := newRollbackManager(t, r, &lastGoodClient{})
		startPreRollback(t, "proceed")
		stubDeployLastGood(t, nil)

		if err := rollback.StoreFor(sm.AppConfig.DataMountPath).PutLastGood(rollbackLastGood(r, "good", rollbackConfig)); err != nil {
			t.Fatal(err)
		}

		recordMemoryFailure(sm, StageDeploy)
		sm.runForkRollback(t.Context(), StageDeploy, errRecorded)

		if f := diskFailure(t, sm); f.State != rollback.StateRolledBack || f.RolledBackTo != r.commits["good"] {
			t.Fatalf("failure %+v, want rolled_back to the last good", f)
		}

		if sm.rollbackPoisoned(t.Context(), "revert-commit", stageLog) {
			t.Fatal("a new commit must deploy")
		}

		if f := diskFailure(t, sm); f.State != rollback.StateFailed || f.RolledBackTo != r.commits["good"] {
			t.Fatalf("failure after the new commit %+v, want failed, still rolled back to the last good", f)
		}

		failure, retry := sm.lastDeploymentFailure()
		if !retry || failure.Stage != string(StageDeploy) {
			t.Fatalf("lastDeploymentFailure() = %+v, %v, want the deploy failure retried", failure, retry)
		}

		if sm.forkRetryRecreates(failure) {
			t.Fatal("the retry after a rolled back failure must not force recreate")
		}

		// After a restart memory is empty and the failure comes from disk: same answer.
		docker.ClearDeploymentFailure(sm.Repository.Name, sm.DeployConfig.Name)

		if failure, _ = sm.lastDeploymentFailure(); sm.forkRetryRecreates(failure) {
			t.Fatal("the retry after a restart must not force recreate either")
		}
	})

	t.Run("blocked", func(t *testing.T) {
		sm := newRollbackManager(t, r, &lastGoodClient{})
		startPreRollback(t, "block")

		if err := rollback.StoreFor(sm.AppConfig.DataMountPath).PutLastGood(rollbackLastGood(r, "good", rollbackConfig)); err != nil {
			t.Fatal(err)
		}

		recordMemoryFailure(sm, StageDeploy)
		sm.runForkRollback(t.Context(), StageDeploy, errRecorded)

		if sm.rollbackPoisoned(t.Context(), "next-commit", stageLog) {
			t.Fatal("a new commit must deploy")
		}

		failure, _ := sm.lastDeploymentFailure()
		if !sm.forkRetryRecreates(failure) {
			t.Fatal("the containers of a blocked failure are the half-applied ones: the retry must force recreate")
		}
	})
}

// forkRetryRecreates keeps the upstream rule unless the disk record is the same failure, failed,
// and rolled back.
func TestForkRetryRecreates(t *testing.T) {
	at := time.Unix(40, 0).UTC()
	failure := docker.DeploymentFailure{CommitSHA: "c2", Stage: "deploy", FailedAt: at}
	repaired := rollback.Failure{Object: "default/web", CommitSHA: "c2", Stage: "deploy", FailedAt: at, State: rollback.StateFailed, RolledBackTo: "c1"}

	cases := map[string]struct {
		rb     *deploy.RollbackConfig
		record *rollback.Failure
		edit   func(*rollback.Failure)
		want   bool
	}{
		"without rollback":             {nil, nil, nil, true},
		"without record":               {&deploy.RollbackConfig{Enabled: true}, nil, nil, true},
		"rolled back":                  {&deploy.RollbackConfig{Enabled: true}, &repaired, nil, false},
		"not rolled back":              {&deploy.RollbackConfig{Enabled: true}, &repaired, func(f *rollback.Failure) { f.RolledBackTo = "" }, true},
		"another commit":               {&deploy.RollbackConfig{Enabled: true}, &repaired, func(f *rollback.Failure) { f.CommitSHA = "c3" }, true},
		"another failure, same commit": {&deploy.RollbackConfig{Enabled: true}, &repaired, func(f *rollback.Failure) { f.FailedAt = at.Add(time.Second) }, true},
		"still poisoned":               {&deploy.RollbackConfig{Enabled: true}, &repaired, func(f *rollback.Failure) { f.State = rollback.StateRolledBack }, true},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			sm, _ := newForkFailureManager(t, tc.rb)

			if tc.record != nil {
				f := *tc.record
				if tc.edit != nil {
					tc.edit(&f)
				}

				putDiskFailure(t, sm, f)
			}

			if got := sm.forkRetryRecreates(failure); got != tc.want {
				t.Fatalf("forkRetryRecreates() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TCK-644: the compose hash the rollback stamps is the one of the last good's content, not of the
// pinned project: it does not depend on how the images were pinned.
func TestRollbackExecutorHashesContentBeforePin(t *testing.T) {
	r := newRollbackRepo(t)

	hashWith := func(apiClient *lastGoodClient) (string, string) {
		t.Helper()

		sm := newRollbackManager(t, r, apiClient)
		got := stubDeployLastGood(t, nil)

		if err := (stageExecutor{s: sm, log: sm.Log}).Deploy(t.Context(), rollbackLastGood(r, "good", rollbackConfig)); err != nil {
			t.Fatal(err)
		}

		return (*got)[0].ProjectHash, (*got)[0].Project.Services["app"].Image
	}

	byRef, refImage := hashWith(&lastGoodClient{refs: map[string]string{"alpine:3.22": "sha256:app", "busybox:1": "sha256:extra"}})
	byID, idImage := hashWith(&lastGoodClient{})

	if refImage != "alpine:3.22" || idImage != "sha256:app" {
		t.Fatalf("app images %q and %q, want one run per pin", refImage, idImage)
	}

	if byRef == "" || byRef != byID {
		t.Fatalf("project hashes %q and %q, want the same non-empty hash of the unpinned project", byRef, byID)
	}
}
