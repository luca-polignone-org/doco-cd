package composepolicy

import (
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// Holder tiene l'ultima compose policy valida e la ricarica quando il file cambia
// (mtime o dimensione). Un file diventato invalido viene ignorato con un log error.
type Holder struct {
	path    string
	log     *slog.Logger
	mu      sync.RWMutex
	current *File
	modTime time.Time // versione del file di current, o dell'ultimo tentativo fallito
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

	data, err := os.ReadFile(h.path) // #nosec G304 -- path dell'operatore
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

// Current restituisce la policy valida più recente; nil se l'Holder è nil (policy spenta).
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
				// Si ricorda la versione fallita, così il reload e il log non si ripetono a ogni
				// deploy finché il file non cambia di nuovo.
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
