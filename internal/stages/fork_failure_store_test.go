package stages

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
)

func newForkFailureManager(t *testing.T, rb *deploy.RollbackConfig) (*StageManager, string) {
	t.Helper()

	sm := newTestStageManager(t)
	data := t.TempDir()
	sm.AppConfig.DataMountPath = data
	sm.DeployConfig.Name = "web"
	sm.DeployConfig.Rollback = rb
	sm.Repository.Name = "org/repo"
	sm.Repository.Revision = "c1"

	t.Cleanup(func() { docker.ClearDeploymentFailure("org/repo", "web") })

	return sm, data
}

// Con rollback abilitato la failure sopravvive alla memoria del processo: si
// registra, si svuota la mappa in memoria (simula un restart) e la si ritrova.
// Senza rollback non si scrive niente sotto <data>/fork/.
func TestFailurePersistedOnlyWithRollback(t *testing.T) {
	// Il failure store in memoria è globale e la chiave è (org/repo, web): niente t.Parallel().
	t.Run("rollback disabled writes nothing", func(t *testing.T) {
		sm, data := newForkFailureManager(t, nil)

		sm.recordDeploymentFailure(StageDeploy, errors.New("x"))

		if _, err := os.Stat(filepath.Join(data, "fork")); !os.IsNotExist(err) {
			t.Fatalf("expected no fork dir, got err=%v", err)
		}
	})

	t.Run("rollback enabled survives restart and clear removes file", func(t *testing.T) {
		sm, data := newForkFailureManager(t, &deploy.RollbackConfig{Enabled: true})

		sm.recordDeploymentFailure(StageDeploy, errors.New("x"))

		file := filepath.Join(data, "fork", "failures", "default", "web.json")
		if _, err := os.Stat(file); err != nil {
			t.Fatalf("expected %s: %v", file, err)
		}

		docker.ClearDeploymentFailure("org/repo", "web")

		failure, ok := sm.lastDeploymentFailure()
		if !ok || failure.CommitSHA != "c1" || failure.Stage != "deploy" {
			t.Fatalf("unexpected failure after restart: %+v ok=%v", failure, ok)
		}

		sm.clearDeploymentFailure()

		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Fatalf("expected file removed, got err=%v", err)
		}

		if _, ok := sm.lastDeploymentFailure(); ok {
			t.Fatal("expected no failure after clear")
		}
	})

	t.Run("unreadable file is treated as blocked", func(t *testing.T) {
		sm, data := newForkFailureManager(t, &deploy.RollbackConfig{Enabled: true})

		file := filepath.Join(data, "fork", "failures", "default", "web.json")
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(file, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}

		if _, ok := sm.lastDeploymentFailure(); !ok {
			t.Fatal("expected a corrupt file to count as a failure")
		}
	})
}
