package stages

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/rollback"
)

// The tests in this file register a process-wide compose policy and use the process-wide job
// gate record, so none of them calls t.Parallel.

// newPoisonedTarget returns a stage manager of default/web after a restart (no job gate verdict)
// whose disk failure has state and the current revision as its commit.
func newPoisonedTarget(t *testing.T, r *rollbackRepo, state rollback.State) *StageManager {
	t.Helper()

	setForkComposePolicy(t, forkPolicyFor("default/web"))
	verifiedObjects.Clear()
	t.Cleanup(func() { verifiedObjects.Clear() })

	sm := newRollbackManager(t, r, &lastGoodClient{})
	sm.Repository.ConfigSourceUrl = forkPolicyRepo
	putDiskFailure(t, sm, rollback.Failure{Object: "default/web", CommitSHA: sm.Repository.Revision, Stage: "deploy", State: state})

	return sm
}

func putLastGood(t *testing.T, sm *StageManager, lg rollback.LastGood) {
	t.Helper()

	if err := rollback.StoreFor(sm.AppConfig.DataMountPath).PutLastGood(lg); err != nil {
		t.Fatal(err)
	}
}

// poisonedRun runs the poison check of pre-deploy and returns its log.
func poisonedRun(t *testing.T, sm *StageManager) string {
	t.Helper()

	var buf bytes.Buffer
	if !sm.rollbackPoisoned(t.Context(), sm.Repository.Revision, slog.New(slog.NewTextHandler(&buf, nil))) {
		t.Fatal("the poisoned commit must be skipped")
	}

	return buf.String()
}

// writeCheckout writes a checkout with compose.yaml and points the repository at it.
func writeCheckout(t *testing.T, sm *StageManager, compose string) {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(compose), 0o600); err != nil {
		t.Fatal(err)
	}

	sm.Repository.PathInternal, sm.Repository.PathExternal = dir, dir
	sm.DeployConfig.ComposeFiles = deploy.New("web", "main").ComposeFiles
}

// After a restart pre-deploy skips the poisoned commit before loading the project, so the job
// gate gets its verdict from the project that actually runs: the last good of a rolled back
// target, the failed commit of a blocked one.
func TestPoisonedTargetJobGateAfterRestart(t *testing.T) {
	r := newRollbackRepo(t)

	t.Run("rolled_back with a compliant last good allows jobs", func(t *testing.T) {
		sm := newPoisonedTarget(t, r, rollback.StateRolledBack)
		got := stubDeployLastGood(t, nil)
		putLastGood(t, sm, rollbackLastGood(r, "good", rollbackConfig))

		if err := forkJobGate("default", "web"); !errors.Is(err, ErrJobNotVerified) {
			t.Fatalf("forkJobGate() before the run = %v, want ErrJobNotVerified", err)
		}

		poisonedRun(t, sm)

		if err := forkJobGate("default", "web"); err != nil {
			t.Fatalf("forkJobGate() = %v, want nil: the running last good passes the policy", err)
		}

		if len(*got) != 0 {
			t.Fatalf("the check deployed the last good %d times", len(*got))
		}
	})

	t.Run("rolled_back with a non compliant last good refuses jobs", func(t *testing.T) {
		sm := newPoisonedTarget(t, r, rollback.StateRolledBack)
		lg := rollbackLastGood(r, "privileged", rollbackConfig)
		lg.Images = lg.Images[:1]
		putLastGood(t, sm, lg)

		if log := poisonedRun(t, sm); strings.Contains(log, "jobs stay refused") {
			t.Fatalf("a policy denial is a verdict, not a failure to log: %q", log)
		}

		if err := forkJobGate("default", "web"); !errors.Is(err, ErrPolicyDenied) {
			t.Fatalf("forkJobGate() = %v, want ErrPolicyDenied", err)
		}
	})

	t.Run("blocked with a compliant running commit allows jobs", func(t *testing.T) {
		sm := newPoisonedTarget(t, r, rollback.StateBlocked)
		writeCheckout(t, sm, "services:\n  app:\n    image: alpine:3.22\n")

		poisonedRun(t, sm)

		if err := forkJobGate("default", "web"); err != nil {
			t.Fatalf("forkJobGate() = %v, want nil: the running commit passes the policy", err)
		}
	})

	t.Run("blocked with a non compliant running commit refuses jobs", func(t *testing.T) {
		sm := newPoisonedTarget(t, r, rollback.StateBlocked)
		writeCheckout(t, sm, "services:\n  app:\n    image: alpine:3.22\n    privileged: true\n")

		poisonedRun(t, sm)

		if err := forkJobGate("default", "web"); !errors.Is(err, ErrPolicyDenied) {
			t.Fatalf("forkJobGate() = %v, want ErrPolicyDenied", err)
		}
	})

	t.Run("rollback_failed records no verdict", func(t *testing.T) {
		sm := newPoisonedTarget(t, r, rollback.StateRollbackFailed)
		putLastGood(t, sm, rollbackLastGood(r, "good", rollbackConfig))

		poisonedRun(t, sm)

		if err := forkJobGate("default", "web"); !errors.Is(err, ErrJobNotVerified) {
			t.Fatalf("forkJobGate() = %v, want ErrJobNotVerified", err)
		}
	})

	t.Run("a verdict already recorded is kept", func(t *testing.T) {
		sm := newPoisonedTarget(t, r, rollback.StateRolledBack)
		putLastGood(t, sm, rollbackLastGood(r, "good", rollbackConfig))
		verifiedObjects.Store("default/web", false)

		poisonedRun(t, sm)

		if err := forkJobGate("default", "web"); !errors.Is(err, ErrPolicyDenied) {
			t.Fatalf("forkJobGate() = %v, want the recorded denial", err)
		}
	})

	t.Run("without a compose policy nothing is checked", func(t *testing.T) {
		sm := newPoisonedTarget(t, r, rollback.StateRolledBack)
		putLastGood(t, sm, rollbackLastGood(r, "good", rollbackConfig))
		SetComposePolicy(nil)

		poisonedRun(t, sm)

		if _, known := verifiedObjects.Load("default/web"); known {
			t.Fatal("a verdict was recorded without a compose policy")
		}
	})

	failures := map[string]func(t *testing.T, sm *StageManager){
		"no last good": func(*testing.T, *StageManager) {},
		"unreadable last good": func(t *testing.T, sm *StageManager) {
			t.Helper()

			path := filepath.Join(sm.AppConfig.DataMountPath, "fork", "lastgood", "default", "web.json")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}

			if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"unsupported source": func(t *testing.T, sm *StageManager) {
			t.Helper()
			putLastGood(t, sm, rollbackLastGood(r, "good", rollbackConfig))
			sm.DeployConfig.RepositoryUrl = "https://example.com/x.git"
		},
		"render error": func(t *testing.T, sm *StageManager) {
			t.Helper()
			putLastGood(t, sm, rollbackLastGood(r, "broken", rollbackConfig))
		},
	}

	for name, setup := range failures {
		t.Run(name+" leaves the jobs refused", func(t *testing.T) {
			sm := newPoisonedTarget(t, r, rollback.StateRolledBack)
			setup(t, sm)

			if log := poisonedRun(t, sm); !strings.Contains(log, "jobs stay refused") {
				t.Fatalf("log %q, want the failed check", log)
			}

			if err := forkJobGate("default", "web"); !errors.Is(err, ErrJobNotVerified) {
				t.Fatalf("forkJobGate() = %v, want ErrJobNotVerified", err)
			}
		})
	}
}

// A corrupt record blocks the revision seen when it was found, and a new commit releases it.
func TestCorruptFailureReleasedByNewCommit(t *testing.T) {
	stageLog := slog.New(slog.NewTextHandler(io.Discard, nil))

	writeCorrupt := func(t *testing.T, data string) string {
		t.Helper()

		path := filepath.Join(data, "fork", "failures", "default", "web.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}

		return path
	}

	t.Run("replaced by a blocked record of the current revision", func(t *testing.T) {
		sm, data := newForkFailureManager(t, &deploy.RollbackConfig{Enabled: true})
		writeCorrupt(t, data)

		if !sm.rollbackPoisoned(t.Context(), "c1", stageLog) {
			t.Fatal("the revision of a corrupt record must stay blocked")
		}

		f := diskFailure(t, sm)
		if f.State != rollback.StateBlocked || f.CommitSHA != "c1" || f.Stage != string(StageDeploy) || f.Stack != "web" || f.FailedAt.IsZero() {
			t.Fatalf("failure on disk %+v, want a blocked record of c1", f)
		}

		if !sm.rollbackPoisoned(t.Context(), "c1", stageLog) {
			t.Fatal("the same revision must stay blocked on the next run")
		}

		if sm.rollbackPoisoned(t.Context(), "c2", stageLog) {
			t.Fatal("a new commit must release the target")
		}

		if f = diskFailure(t, sm); f.State != rollback.StateFailed || f.CommitSHA != "c1" {
			t.Fatalf("failure on disk %+v, want failed with the old commit", f)
		}
	})

	t.Run("a failed replace is logged and still blocks", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}

		sm, data := newForkFailureManager(t, &deploy.RollbackConfig{Enabled: true})
		dir := filepath.Dir(writeCorrupt(t, data))

		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

		buf := captureLog(sm)

		if f, ok := sm.loadForkFailure(); !ok || f.State != rollback.StateBlocked || f.CommitSHA != "c1" {
			t.Fatalf("loadForkFailure() = %+v, %v, want blocked on c1", f, ok)
		}

		if !strings.Contains(buf.String(), "failed to replace corrupt deployment failure") {
			t.Fatalf("log %q", buf.String())
		}
	})
}
