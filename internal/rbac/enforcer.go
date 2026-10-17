package rbac

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// Enforcer keeps the last valid policy of a file and reloads it when the file's
// mtime or size changes. A nil *Enforcer means RBAC is off: everything is allowed,
// as upstream.
//
// Contract with the policy writer (Portico): the file must be written atomically
// (temp file + rename). A file truncated at a line boundary is still a valid CSV
// that just lacks the last lines, and the enforcer cannot detect that.
type Enforcer struct {
	path     string
	log      *slog.Logger
	mu       sync.RWMutex // guards policy, modTime, size, missing
	reloadMu sync.Mutex   // serializes reloads, one reload and one log per version
	missing  bool
	policy   *Policy
	modTime  time.Time
	size     int64
}

// NewFileEnforcer loads the policy at path; the initial load must succeed.
func NewFileEnforcer(path string, log *slog.Logger) (*Enforcer, error) {
	e := &Enforcer{path: path, log: log}
	if err := e.reload(); err != nil {
		return nil, err
	}

	return e, nil
}

func (e *Enforcer) reload() error {
	info, err := os.Stat(e.path)
	if err != nil {
		return fmt.Errorf("stat rbac policy %s: %w", e.path, err)
	}

	data, err := os.ReadFile(e.path) // #nosec G304 -- operator-supplied path
	if err != nil {
		return fmt.Errorf("read rbac policy %s: %w", e.path, err)
	}

	p, err := Parse(bytes.NewReader(data))
	if err != nil {
		return err
	}

	e.mu.Lock()
	e.policy, e.modTime, e.size = p, info.ModTime(), info.Size()
	e.mu.Unlock()

	return nil
}

// Enabled reports whether RBAC is on, that is whether the enforcer is non-nil.
func (e *Enforcer) Enabled() bool { return e != nil }

// changed reports whether info differs from the loaded version, and records that
// the file is present.
func (e *Enforcer) changed(info os.FileInfo) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.missing = false

	return !info.ModTime().Equal(e.modTime) || info.Size() != e.size
}

func (e *Enforcer) current() *Policy {
	info, err := os.Stat(e.path)

	switch {
	case err != nil:
		e.mu.Lock()
		first := !e.missing
		e.missing = true
		e.mu.Unlock()

		if first {
			e.log.Error("rbac policy file missing, keeping last valid policy", slog.Any("error", err))
		}
	case e.changed(info):
		e.reloadMu.Lock()
		// Re-check: a concurrent request may have already handled this version.
		if e.changed(info) {
			if err = e.reload(); err != nil {
				e.mu.Lock()
				e.modTime, e.size = info.ModTime(), info.Size() // do not retry the same version
				e.mu.Unlock()
				e.log.Error("rbac policy reload failed, keeping last valid policy", slog.Any("error", err))
			}
		}

		e.reloadMu.Unlock()
	}

	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.policy
}

// Enforce decides and writes the audit log: every deny, and every allow on an
// action other than get.
func (e *Enforcer) Enforce(pr Principal, resource, action, object string) bool {
	if e == nil {
		return true
	}

	allowed := e.current().Allowed(pr, resource, action, object)

	if !allowed || action != "get" {
		decision := "allow"
		if !allowed {
			decision = "deny"
		}

		e.log.Info("rbac decision",
			slog.String("sub", pr.Subject), slog.Any("groups", pr.Groups),
			slog.String("resource", resource), slog.String("action", action),
			slog.String("object", object), slog.String("decision", decision))
	}

	return allowed
}
