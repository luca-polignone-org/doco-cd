package rollback

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/kimdre/doco-cd/internal/filesystem"
)

// The last good is the last successful deploy of a target: commit, repository, deploy config
// without its internal part (which holds the resolved secrets) and the effective images. The
// repository files are not copied: the commit stays reachable in the mirror (git.PinLastGood)
// and the rollback republishes its artifact, which holds everything the deploy found.

// ServiceImage is the effective image of one service of the deployed project.
type ServiceImage struct {
	Service     string   `json:"service"`
	Image       string   `json:"image"`
	ImageID     string   `json:"image_id"`
	RepoDigests []string `json:"repo_digests,omitempty"`
}

// LastGood is the recorded state of the last successful deploy of a target.
type LastGood struct {
	Version      int            `json:"version"`
	Object       string         `json:"object"`
	CommitSHA    string         `json:"commit_sha"`
	SourceURL    string         `json:"source_url"`
	ProjectHash  string         `json:"project_hash"`
	DeployConfig []byte         `json:"deploy_config"` // YAML of deploy.Config with Internal zeroed
	Images       []ServiceImage `json:"images"`
	RecordedAt   time.Time      `json:"recorded_at"`
}

func (s *Store) PutLastGood(lg LastGood) error {
	path, err := s.objectPath("lastgood", lg.Object, ".json")
	if err != nil {
		return err
	}

	lg.Version = storeVersion

	data, err := json.Marshal(lg)
	if err != nil {
		return fmt.Errorf("encode last good %s: %w", lg.Object, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return filesystem.AtomicWriteFile(path, data, filesystem.PermPrivateFile)
}

func (s *Store) GetLastGood(object string) (LastGood, bool, error) {
	path, err := s.objectPath("lastgood", object, ".json")
	if err != nil {
		return LastGood{}, false, err
	}

	data, err := os.ReadFile(path) // #nosec G304 -- path built from PathEscape under root
	if errors.Is(err, os.ErrNotExist) {
		return LastGood{}, false, nil
	}

	if err != nil {
		return LastGood{}, false, fmt.Errorf("read last good %s: %w", object, err)
	}

	var lg LastGood
	if err = json.Unmarshal(data, &lg); err != nil {
		return LastGood{}, false, fmt.Errorf("decode last good %s: %w", object, err)
	}

	if lg.Version != storeVersion {
		return LastGood{}, false, fmt.Errorf("last good %s: unsupported version %d", object, lg.Version)
	}

	return lg, true, nil
}
