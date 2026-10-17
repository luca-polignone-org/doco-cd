package stages

import (
	"log/slog"

	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/rollback"
)

// forkStore returns the failure store only if the target has rollback enabled:
// otherwise the fork writes nothing under <data>/fork/ (spec, "Configuration").
func (s *StageManager) forkStore() *rollback.Store {
	if !s.DeployConfig.RollbackEnabled() || s.AppConfig == nil {
		return nil
	}

	return rollback.StoreFor(s.AppConfig.DataMountPath)
}

func (s *StageManager) persistForkFailure(f docker.DeploymentFailure, state rollback.State, decision string) {
	store := s.forkStore()
	if store == nil {
		return
	}

	err := store.PutFailure(rollback.Failure{
		Object: s.DeployConfig.Object(), Repository: f.Repository, Stack: f.Stack,
		CommitSHA: f.CommitSHA, Stage: f.Stage, Error: truncateRunes(f.Error, 2000),
		FailedAt: f.FailedAt, State: state, Decision: decision,
	})
	if err != nil {
		s.Log.Error("failed to persist deployment failure", slog.String("object", s.DeployConfig.Object()), slog.Any("error", err))
	}
}

// loadForkFailure reads the failure from disk. An unreadable file counts as a
// blocked failure: never a blind deploy on top of a state that cannot be read.
func (s *StageManager) loadForkFailure() (rollback.Failure, bool) {
	store := s.forkStore()
	if store == nil {
		return rollback.Failure{}, false
	}

	f, ok, err := store.GetFailure(s.DeployConfig.Object())
	if err != nil {
		s.Log.Error("failed to read deployment failure, treating target as blocked",
			slog.String("object", s.DeployConfig.Object()), slog.Any("error", err))

		return rollback.Failure{Object: s.DeployConfig.Object(), State: rollback.StateBlocked, CommitSHA: s.Repository.Revision}, true
	}

	return f, ok
}

func (s *StageManager) deleteForkFailure() {
	if store := s.forkStore(); store != nil {
		if err := store.DeleteFailure(s.DeployConfig.Object()); err != nil {
			s.Log.Error("failed to delete deployment failure", slog.Any("error", err))
		}
	}
}

func truncateRunes(v string, n int) string {
	if r := []rune(v); len(r) > n {
		return string(r[:n])
	}

	return v
}
