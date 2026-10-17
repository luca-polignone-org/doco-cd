package composepolicy

import (
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// Holder keeps the last valid compose policy and reloads it when the file changes
// (mtime or size). A file that became invalid is ignored with an error log.
type Holder struct {
	path    string
	log     *slog.Logger
	mu      sync.RWMutex
	current *File
	modTime time.Time // file version of current, or of the last failed attempt
	size    int64
}

func NewHolder(path string, log *slog.Logger) (*Holder, error) {
	h := &Holder{path: path, log: log}
	if err := h.reload(); err != nil {
		return nil, err
	}

	return h, nil
}

func (h *Holder) reload() error {
	info, err := os.Stat(h.path)
	if err != nil {
		return fmt.Errorf("stat compose policy %s: %w", h.path, err)
	}

	data, err := os.ReadFile(h.path) // #nosec G304 -- operator-supplied path
	if err != nil {
		return fmt.Errorf("read compose policy %s: %w", h.path, err)
	}

	f, err := Parse(data)
	if err != nil {
		return err
	}

	h.mu.Lock()
	h.current, h.modTime, h.size = f, info.ModTime(), info.Size()
	h.mu.Unlock()

	return nil
}

// Current returns the most recent valid policy; nil if the Holder is nil (policy disabled).
func (h *Holder) Current() *File {
	if h == nil {
		return nil
	}

	if info, err := os.Stat(h.path); err == nil {
		h.mu.RLock()
		changed := !info.ModTime().Equal(h.modTime) || info.Size() != h.size
		h.mu.RUnlock()

		if changed {
			if err = h.reload(); err != nil {
				// Remember the failed version so the reload and the log are not repeated on
				// every deploy until the file changes again.
				h.mu.Lock()
				h.modTime, h.size = info.ModTime(), info.Size()
				h.mu.Unlock()
				h.log.Error("compose policy reload failed, keeping last valid policy", slog.Any("error", err))
			}
		}
	}

	h.mu.RLock()
	defer h.mu.RUnlock()

	return h.current
}
