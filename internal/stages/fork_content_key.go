package stages

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"

	gitInternal "github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/rollback"
)

// The content key (rollback.unblock_on: content, spec 3c) tells a new commit that changes the target
// from one that only moved the branch: in a monorepo of deploy configs a bump of another app makes a
// new commit for every target, and with the commit rule it would release a condemned digest. The key
// is the git tree hash of working_dir at the revision, from the deploy mirror, plus the git blob hash
// of the deploy config file read from disk. It needs neither the vault nor the compose project.
// Note the order in pre-deploy: external secrets are resolved before rollbackPoisoned, so with
// OpenBao unreachable the run already fails there and the key is not reached; runForkDeployStage
// checks the poison again under the target lock.

var errNoContentKey = errors.New("content key unavailable")

// forkContentKey computes the key of revision: the revision rollbackPoisoned is asked about, never
// a field of the run that could point elsewhere.
func (s *StageManager) forkContentKey(revision string) (string, error) {
	if s.Repository.MirrorDir == "" {
		return "", fmt.Errorf("%w: no git mirror (OCI source)", errNoContentKey)
	}

	if s.DeployConfig.Internal.File == "" {
		return "", fmt.Errorf("%w: deploy config file unknown", errNoContentKey)
	}

	tree, err := gitInternal.TreeHashAt(s.Repository.MirrorDir, strings.TrimSpace(revision), s.DeployConfig.WorkingDirectory)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errNoContentKey, err)
	}

	data, err := os.ReadFile(s.DeployConfig.Internal.File)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errNoContentKey, err)
	}

	return "tree:" + tree + ";config:" + plumbing.ComputeHash(plumbing.BlobObject, data).String(), nil
}

// forkContentKeyOrEmpty is the key a failure record carries: empty in commit mode, and empty (with a
// warning) when it cannot be computed, in which case the record releases on a new commit as before.
func (s *StageManager) forkContentKeyOrEmpty() string {
	if !s.DeployConfig.Rollback.UnblocksOnContent() {
		return ""
	}

	key, err := s.forkContentKey(s.Repository.Revision)
	if err != nil {
		s.Log.Warn("rollback content key not recorded, the failure releases on a new commit", slog.Any("error", err))
		return ""
	}

	return key
}

// sameForkContent reports whether a new commit leaves the content of a poisoned target unchanged. It
// is false in commit mode and for a record without a key. A key that cannot be computed counts as the
// same content: never a blind redeploy of a condemned commit.
func (s *StageManager) sameForkContent(f rollback.Failure, revision string, stageLog *slog.Logger) bool {
	if !s.DeployConfig.Rollback.UnblocksOnContent() || f.ContentKey == "" {
		return false
	}

	key, err := s.forkContentKey(revision)
	if err != nil {
		stageLog.Error("cannot compute the rollback content key, keeping the target poisoned", slog.Any("error", err))
		return true
	}

	return key == f.ContentKey
}
