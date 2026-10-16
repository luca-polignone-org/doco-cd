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

// Il failure store del fork tiene su disco l'esito dell'ultimo deploy fallito di un
// target con rollback abilitato. Upstream lo tiene solo in memoria
// (internal/docker/failure_state.go) e lo perde al restart; qui serve che sopravviva,
// perché uno stato rolled_back o blocked deve impedire di rideployare il commit
// fallito anche dopo un riavvio.

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

// StoreFor restituisce lo store condiviso della directory dati: un'istanza per path,
// così il mutex serializza davvero le scritture dello stesso processo.
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

func splitObject(object string) (string, string, error) {
	ctx, project, ok := strings.Cut(object, "/")
	if !ok || ctx == "" || project == "" || strings.Contains(project, "/") {
		return "", "", fmt.Errorf("%w: %q", errBadObject, object)
	}

	// url.PathEscape non codifica "." e "..": come componenti di path uscirebbero dallo store.
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

	data, err := os.ReadFile(path) // #nosec G304 -- path costruito da PathEscape sotto root
	if errors.Is(err, os.ErrNotExist) {
		return Failure{}, false, nil
	}

	if err != nil {
		return Failure{}, false, fmt.Errorf("read failure %s: %w", object, err)
	}

	var f Failure
	if err = json.Unmarshal(data, &f); err != nil {
		return Failure{}, false, fmt.Errorf("decode failure %s: %w", object, err)
	}

	if f.Version != storeVersion {
		return Failure{}, false, fmt.Errorf("failure %s: unsupported version %d", object, f.Version)
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

// Blocks dice se la failure impedisce di rideployare il commit fallito.
func (s *Store) Blocks(f Failure) bool {
	return f.State != StateFailed
}
