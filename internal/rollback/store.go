package rollback

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kimdre/doco-cd/internal/filesystem"
)

// The fork's failure store keeps the outcome of the last failed deploy of a
// target with rollback enabled on disk. Upstream keeps it in memory only
// (internal/docker/failure_state.go) and loses it on restart; here it must survive,
// because a rolled_back or blocked state must prevent redeploying the failed commit
// even after a restart.

type State string

const (
	StateFailed         State = "failed"
	StateRolledBack     State = "rolled_back"
	StateBlocked        State = "blocked"
	StateRollbackFailed State = "rollback_failed"
)

const storeVersion = 1

type Failure struct {
	Version    int       `json:"version"`
	Object     string    `json:"object"`
	Repository string    `json:"repository"`
	Stack      string    `json:"stack"`
	CommitSHA  string    `json:"commit_sha"`
	Stage      string    `json:"stage"`
	Error      string    `json:"error"`
	FailedAt   time.Time `json:"failed_at"`
	State      State     `json:"state"`
	Decision   string    `json:"decision,omitempty"`
}

type Store struct {
	root string
	mu   sync.Mutex
}

var (
	storesMu sync.Mutex
	stores   = map[string]*Store{}
)

// StoreFor returns the store shared for the data directory: one instance per path,
// so the mutex really serializes writes within the same process.
func StoreFor(dataMountPath string) *Store {
	root := filepath.Join(filepath.Clean(dataMountPath), "fork")

	storesMu.Lock()
	defer storesMu.Unlock()

	if s, ok := stores[root]; ok {
		return s
	}

	s := &Store{root: root}
	stores[root] = s

	return s
}

func (s *Store) Root() string { return s.root }

var errBadObject = errors.New("object must be <context>/<project>")

// ErrCorrupt marks a record that was read but cannot be decoded or has an unknown version, as
// opposed to an I/O error reading it.
var ErrCorrupt = errors.New("corrupt record")

func splitObject(object string) (string, string, error) {
	ctx, project, ok := strings.Cut(object, "/")
	if !ok || ctx == "" || project == "" || strings.Contains(project, "/") {
		return "", "", fmt.Errorf("%w: %q", errBadObject, object)
	}

	// url.PathEscape does not encode "." and "..": as path components they would escape the store.
	for _, part := range []string{ctx, project} {
		if part == "." || part == ".." {
			return "", "", fmt.Errorf("%w: %q", errBadObject, object)
		}
	}

	return ctx, project, nil
}

func (s *Store) objectPath(kind, object, suffix string) (string, error) {
	ctx, project, err := splitObject(object)
	if err != nil {
		return "", err
	}

	return filepath.Join(s.root, kind, url.PathEscape(ctx), url.PathEscape(project)+suffix), nil
}

func (s *Store) PutFailure(f Failure) error {
	path, err := s.objectPath("failures", f.Object, ".json")
	if err != nil {
		return err
	}

	f.Version = storeVersion

	data, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("encode failure %s: %w", f.Object, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return filesystem.AtomicWriteFile(path, data, filesystem.PermPrivateFile)
}

func (s *Store) GetFailure(object string) (Failure, bool, error) {
	path, err := s.objectPath("failures", object, ".json")
	if err != nil {
		return Failure{}, false, err
	}

	data, err := os.ReadFile(path) // #nosec G304 -- path built from PathEscape under root
	if errors.Is(err, os.ErrNotExist) {
		return Failure{}, false, nil
	}

	if err != nil {
		return Failure{}, false, fmt.Errorf("read failure %s: %w", object, err)
	}

	var f Failure
	if err = json.Unmarshal(data, &f); err != nil {
		return Failure{}, false, fmt.Errorf("%w: decode failure %s: %w", ErrCorrupt, object, err)
	}

	if f.Version != storeVersion {
		return Failure{}, false, fmt.Errorf("%w: failure %s: unsupported version %d", ErrCorrupt, object, f.Version)
	}

	return f, true, nil
}

func (s *Store) DeleteFailure(object string) error {
	path, err := s.objectPath("failures", object, ".json")
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err = os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete failure %s: %w", object, err)
	}

	return nil
}

// Blocks reports whether the failure prevents redeploying the failed commit.
func (s *Store) Blocks(f Failure) bool {
	return f.State != StateFailed
}
