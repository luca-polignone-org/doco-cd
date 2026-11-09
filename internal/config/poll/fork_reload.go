package poll

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v4"

	"github.com/kimdre/doco-cd/internal/config"
)

// POLL_CONFIG_FILE reload (Portico fork, poli.7). Upstream reads POLL_CONFIG_FILE once at start. The
// Portico reconciler rewrites the file whenever platform-config changes, so that a retired
// application leaves the poll. Here the file is re-read when it changes (config.ReloadingFile,
// checked every ConfigFileReloadInterval), the new content is parsed and validated as at start
// (ParseConfigs), and a JobSet makes the running poll jobs follow the entries: a removed entry
// stops its job after the run in progress, a new entry starts one, an unchanged entry is not
// touched. Content that does not parse or validate keeps the previous entries, with an error log.

// ConfigFileReloadInterval is how often POLL_CONFIG_FILE is checked for changes (a stat, plus a
// read only when the file changed).
const ConfigFileReloadInterval = 5 * time.Second

// ParseConfigs parses a POLL_CONFIG document and validates every entry, as GetConfig does at start.
// An empty document is an empty configuration.
func ParseConfigs(content string) ([]Config, error) {
	var configs []Config

	if err := yaml.Unmarshal([]byte(content), &configs); err != nil {
		return nil, fmt.Errorf("failed to parse poll config: %w", err)
	}

	for i := range configs {
		if err := configs[i].Validate(); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
		}
	}

	if configs == nil {
		configs = []Config{}
	}

	return configs, nil
}

// JobStarter starts the poll job of cfg in its own goroutine and returns. The job must end after
// its run in progress once stop is closed.
type JobStarter func(cfg Config, stop <-chan struct{}) error

// JobChanges counts what an Apply did to the running poll jobs.
type JobChanges struct {
	Started, Stopped, Kept int
}

// JobSet keeps one poll job per entry of the poll configuration. It is safe for concurrent use.
type JobSet struct {
	start JobStarter

	mu   sync.Mutex
	jobs []setJob
}

type setJob struct {
	key  string // the entry the job was started with, see jobKey
	stop chan struct{}
}

// NewJobSet returns an empty set that starts its jobs with start.
func NewJobSet(start JobStarter) *JobSet {
	return &JobSet{start: start}
}

// Apply makes the running jobs follow configs. Entries are matched by value, one job per entry, so
// two identical entries keep two jobs. A job without a matching entry is stopped; an entry without a
// matching job gets a new one. A job that fails to start is left out of the set, so the next Apply
// tries again, and its error is returned after the other entries are handled.
func (s *JobSet) Apply(configs []Config) (JobChanges, error) {
	keys := make([]string, len(configs))
	for i := range configs {
		keys[i] = jobKey(configs[i])
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var changes JobChanges

	kept := make([]bool, len(s.jobs))
	toStart := make([]int, 0, len(configs))

	for i, key := range keys {
		matched := false

		for j, job := range s.jobs {
			if !kept[j] && job.key == key {
				kept[j], matched = true, true
				changes.Kept++

				break
			}
		}

		if !matched {
			toStart = append(toStart, i)
		}
	}

	next := make([]setJob, 0, len(configs))

	for j, job := range s.jobs {
		if kept[j] {
			next = append(next, job)

			continue
		}

		close(job.stop)

		changes.Stopped++
	}

	var errs []error

	for _, i := range toStart {
		stop := make(chan struct{})

		if err := s.start(configs[i], stop); err != nil {
			errs = append(errs, err)

			continue
		}

		next = append(next, setJob{key: keys[i], stop: stop})
		changes.Started++
	}

	s.jobs = next

	return changes, errors.Join(errs...)
}

// jobKey identifies an entry by its user-set fields. It is taken when the job starts, so a run that
// later fills the Internal fields of an inline deployment (excluded from JSON) does not change it.
func jobKey(cfg Config) string {
	// Config and deploy.Config hold only types encoding/json encodes (no channel, func or complex).
	data, _ := json.Marshal(cfg) //nolint:errchkjson // see above

	return string(data)
}

// WatchConfigFile checks file every interval until ctx is done. When its content differs from the
// last one seen (initially current, the content parsed at start), the content is parsed with
// ParseConfigs and handed to apply; content that does not parse or validate keeps the previous
// configuration and is logged once, at error level.
func WatchConfigFile(ctx context.Context, file *config.ReloadingFile, current string, interval time.Duration,
	apply func([]Config) error, log *slog.Logger,
) {
	if log == nil {
		log = slog.Default()
	}

	last := strings.TrimSpace(current)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		content := file.Value()
		if content == last {
			continue
		}

		last = content

		configs, err := ParseConfigs(content)
		if err != nil {
			log.Error("POLL_CONFIG_FILE content is invalid, keeping the previous poll configuration",
				slog.String("error", err.Error()))

			continue
		}

		if err := apply(configs); err != nil {
			log.Error("POLL_CONFIG_FILE applied with errors", slog.String("error", err.Error()))
		}
	}
}
