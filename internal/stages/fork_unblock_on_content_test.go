package stages

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	gitInternal "github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/rollback"
)

// refreshMirror fetches the commits made in r after newRollbackManager cloned the mirror. Without
// it the mirror lacks the new revision, forkContentKey fails, and a "keeps the poison" case would
// pass through the fail-closed path instead of the content comparison.
func refreshMirror(t *testing.T, sm *StageManager) {
	t.Helper()

	gitStore, err := sm.newGitStore(sm.Log, filepath.Dir(sm.Repository.MirrorDir))
	if err != nil {
		t.Fatal(err)
	}

	if _, err = gitStore.Resolve(t.Context(), gitInternal.MainBranch); err != nil {
		t.Fatalf("refresh mirror: %v", err)
	}
}

// commitAndRefresh commits files in r and makes the mirror see the commit.
func commitAndRefresh(t *testing.T, r *rollbackRepo, sm *StageManager, name string, files map[string]string) string {
	t.Helper()

	c := r.commit(t, name, files)
	refreshMirror(t, sm)

	return c
}

// newContentTarget returns a rolled_back target of web, unblock_on: content, whose failure was
// recorded at the "good" commit of r with its content key, and whose deploy config file is cfgPath.
func newContentTarget(t *testing.T, r *rollbackRepo) (*StageManager, string) {
	t.Helper()

	sm := newRollbackManager(t, r, &lastGoodClient{})
	sm.DeployConfig.Rollback = &deploy.RollbackConfig{Enabled: true, UnblockOn: deploy.UnblockOnContent}
	sm.DeployConfig.WorkingDirectory = "deploy"

	cfgPath := filepath.Join(t.TempDir(), ".doco-cd.yml")
	if err := os.WriteFile(cfgPath, []byte(rollbackConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	sm.DeployConfig.Internal.File = cfgPath

	key, err := sm.forkContentKey(r.commits["good"])
	if err != nil {
		t.Fatalf("forkContentKey() error = %v", err)
	}

	putDiskFailure(t, sm, rollback.Failure{
		Object: "default/web", CommitSHA: r.commits["good"], Stage: "deploy",
		State: rollback.StateRolledBack, ContentKey: key,
	})

	return sm, cfgPath
}

// poisonedWithLog runs rollbackPoisoned on revision and returns its verdict and log.
func poisonedWithLog(t *testing.T, sm *StageManager, revision string) (bool, string) {
	t.Helper()

	var buf bytes.Buffer

	got := sm.rollbackPoisoned(t.Context(), revision, slog.New(slog.NewTextHandler(&buf, nil)))

	return got, buf.String()
}

func TestUnblockOnContent(t *testing.T) {
	otherApp := mergeFiles(rollbackGoodFiles, map[string]string{"other/compose.yaml": "services: {}\n"})

	t.Run("a commit of another directory keeps the poison", func(t *testing.T) {
		r := newRollbackRepo(t)
		sm, _ := newContentTarget(t, r)

		other := commitAndRefresh(t, r, sm, "other app", otherApp)

		got, log := poisonedWithLog(t, sm, other)
		if !got {
			t.Fatal("same content: the new commit must stay poisoned")
		}

		// The verdict must come from the comparison, not from a key that could not be computed.
		if strings.Contains(log, "cannot compute") {
			t.Fatalf("the content key must be computable here, log: %s", log)
		}

		if f := diskFailure(t, sm); f.State != rollback.StateRolledBack || f.CommitSHA != r.commits["good"] {
			t.Fatalf("failure on disk %+v, want unchanged", f)
		}
	})

	t.Run("a commit of the working directory releases", func(t *testing.T) {
		r := newRollbackRepo(t)
		sm, _ := newContentTarget(t, r)

		changed := commitAndRefresh(t, r, sm, "new compose",
			mergeFiles(rollbackGoodFiles, map[string]string{"deploy/compose.yaml": "services:\n  app:\n    image: alpine:3.23\n"}))

		got, log := poisonedWithLog(t, sm, changed)
		if got || strings.Contains(log, "cannot compute") {
			t.Fatalf("new content must release the target (poisoned=%v), log: %s", got, log)
		}

		if f := diskFailure(t, sm); f.State != rollback.StateFailed {
			t.Fatalf("failure on disk %+v, want failed", f)
		}
	})

	t.Run("a changed deploy config releases", func(t *testing.T) {
		r := newRollbackRepo(t)
		sm, cfgPath := newContentTarget(t, r)

		other := commitAndRefresh(t, r, sm, "other app", otherApp)

		if err := os.WriteFile(cfgPath, []byte(rollbackConfig+"force_image_pull: true\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		if got, log := poisonedWithLog(t, sm, other); got || strings.Contains(log, "cannot compute") {
			t.Fatalf("a new deploy config must release the target (poisoned=%v), log: %s", got, log)
		}
	})

	t.Run("unreadable mirror fails closed", func(t *testing.T) {
		r := newRollbackRepo(t)
		sm, _ := newContentTarget(t, r)

		other := commitAndRefresh(t, r, sm, "other app", otherApp)
		sm.Repository.MirrorDir = filepath.Join(t.TempDir(), "gone")

		got, log := poisonedWithLog(t, sm, other)
		if !got {
			t.Fatal("a key that cannot be computed must keep the poison")
		}

		if !strings.Contains(log, "cannot compute the rollback content key") {
			t.Fatalf("log must say why: %s", log)
		}
	})

	t.Run("a v1 record without key releases on commit", func(t *testing.T) {
		r := newRollbackRepo(t)
		sm, _ := newContentTarget(t, r)
		putDiskFailure(t, sm, rollback.Failure{Object: "default/web", CommitSHA: r.commits["good"], Stage: "deploy", State: rollback.StateRolledBack})

		other := commitAndRefresh(t, r, sm, "other app", otherApp)
		if got, _ := poisonedWithLog(t, sm, other); got {
			t.Fatal("without a recorded key the commit rule applies")
		}
	})

	t.Run("commit mode is unchanged", func(t *testing.T) {
		r := newRollbackRepo(t)
		sm, _ := newContentTarget(t, r)
		sm.DeployConfig.Rollback.UnblockOn = ""

		other := commitAndRefresh(t, r, sm, "other app", otherApp)
		if got, _ := poisonedWithLog(t, sm, other); got {
			t.Fatal("commit mode must release on any new commit")
		}
	})
}

func TestForkContentKeyErrors(t *testing.T) {
	r := newRollbackRepo(t)
	sm, _ := newContentTarget(t, r)
	good := r.commits["good"]

	sm.DeployConfig.Internal.File = filepath.Join(t.TempDir(), "missing.yml")
	if _, err := sm.forkContentKey(good); err == nil {
		t.Fatal("an unreadable deploy config must be an error")
	}

	sm.DeployConfig.Internal.File = ""
	if _, err := sm.forkContentKey(good); err == nil {
		t.Fatal("a config without a file must be an error")
	}

	sm.Repository.MirrorDir = ""
	if _, err := sm.forkContentKey(good); err == nil {
		t.Fatal("a target without a mirror (OCI) must be an error")
	}
}

func TestFailureRecordsCarryContentKey(t *testing.T) {
	r := newRollbackRepo(t)
	sm, _ := newContentTarget(t, r)
	sm.Repository.Revision = r.commits["good"]

	want, _ := sm.forkContentKey(r.commits["good"])
	if got := sm.forkContentKeyOrEmpty(); got != want || got == "" {
		t.Fatalf("forkContentKeyOrEmpty() = %q, want %q", got, want)
	}

	sm.persistForkFailure(docker.DeploymentFailure{CommitSHA: r.commits["good"], Stage: "deploy"}, rollback.StateBlocked, "")

	if f := diskFailure(t, sm); f.ContentKey != want {
		t.Fatalf("persisted failure key = %q, want %q", f.ContentKey, want)
	}

	sm.DeployConfig.Rollback.UnblockOn = ""
	if got := sm.forkContentKeyOrEmpty(); got != "" {
		t.Fatalf("commit mode records no key, got %q", got)
	}

	sm.DeployConfig.Rollback.UnblockOn = deploy.UnblockOnContent
	sm.Repository.MirrorDir = filepath.Join(t.TempDir(), "gone")

	if got := sm.forkContentKeyOrEmpty(); got != "" {
		t.Fatalf("a key that cannot be computed is not recorded, got %q", got)
	}
}

func mergeFiles(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}

	for k, v := range b {
		out[k] = v
	}

	return out
}

func TestRunForkRollbackRecordsContentKey(t *testing.T) {
	r := newRollbackRepo(t)
	sm, _ := newContentTarget(t, r)
	sm.Repository.Revision = r.commits["good"]

	startPreRollback(t, "proceed")
	stubDeployLastGood(t, nil)

	want, err := sm.forkContentKey(r.commits["good"])
	if err != nil {
		t.Fatal(err)
	}

	if err = rollback.StoreFor(sm.AppConfig.DataMountPath).PutLastGood(rollbackLastGood(r, "good", rollbackConfig)); err != nil {
		t.Fatal(err)
	}

	recordMemoryFailure(sm, StageDeploy)
	sm.runForkRollback(t.Context(), StageDeploy, errRecorded)

	if f := diskFailure(t, sm); f.ContentKey != want {
		t.Fatalf("rollback failure key = %q, want %q", f.ContentKey, want)
	}
}

func TestPersistForkFailureStoreError(t *testing.T) {
	r := newRollbackRepo(t)
	sm, _ := newContentTarget(t, r)
	sm.Repository.Revision = r.commits["good"]

	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	sm.AppConfig.DataMountPath = notADir
	logs := captureLog(sm)

	sm.persistForkFailure(docker.DeploymentFailure{CommitSHA: r.commits["good"], Stage: "deploy"}, rollback.StateBlocked, "")

	if !strings.Contains(logs.String(), "failed to persist deployment failure") {
		t.Fatalf("a store error must be logged: %s", logs.String())
	}
}
