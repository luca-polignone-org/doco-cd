package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/kimdre/doco-cd/internal/encryption"
)

// Reloading *_FILE values (Portico fork, poli.6). Upstream reads a *_FILE variable once, in
// LoadFileBasedEnvVars. ReloadingFile serves the same value (trimmed and SOPS-decrypted as
// upstream does) and re-reads the file whenever its identity, mtime or size changed, so a
// short-lived token rewritten with an atomic rename is used without a restart.
//
// A missing, unreadable, empty or undecryptable file keeps the last good value: the token is
// opaque and its expiry cannot be known here, and an old token that may still be valid is never
// worse than no token (an expired one gets the same 401 as none). A failure clears the stamp, so the
// file is read again on every use until a read succeeds: a chmod that fixes the permissions does not
// move the mtime, and a file recreated after a removal may reuse the old inode, size and mtime. The logs carry the variable, the path and the age of the value, never the value.

// ReloadingFile is safe for concurrent use.
type ReloadingFile struct {
	envName string
	path    string
	log     *slog.Logger

	mu       sync.Mutex
	value    string
	info     os.FileInfo // the version value was read from
	loadedAt time.Time
	failure  string // the failure last logged; "" after a good read
}

// NewReloadingFile reads envName+"_FILE" at path. An empty file is a valid empty value, as
// upstream; a missing or unreadable one is an error.
func NewReloadingFile(envName, path string, log *slog.Logger) (*ReloadingFile, error) {
	if log == nil {
		log = slog.Default()
	}

	f := &ReloadingFile{envName: envName, path: path, log: log}

	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("%s_FILE: %w", envName, err)
	}

	value, err := f.read()
	if err != nil {
		return nil, err
	}

	f.value, f.info, f.loadedAt = value, info, time.Now()

	return f, nil
}

// Value returns the current value, reloading the file first if it changed.
func (f *ReloadingFile) Value() string {
	f.mu.Lock()
	defer f.mu.Unlock()

	info, err := os.Stat(f.path)
	if err != nil {
		f.failLocked("missing", err)

		return f.value
	}

	if f.info != nil && os.SameFile(info, f.info) && info.ModTime().Equal(f.info.ModTime()) && info.Size() == f.info.Size() {
		// Same version as the one in use. After a failure f.info is nil, so a failed file never lands here.
		return f.value
	}

	value, err := f.read()

	switch {
	case err != nil:
		f.failLocked("unreadable", err)
	case value == "":
		f.failLocked("empty", nil)
	default:
		f.value, f.info, f.loadedAt = value, info, time.Now()
		f.recoveredLocked()
		f.log.Info(f.envName+"_FILE reloaded", slog.String("path", f.path))
	}

	return f.value
}

func (f *ReloadingFile) read() (string, error) {
	data, err := os.ReadFile(f.path) // #nosec G304 -- operator-configured *_FILE path
	if err != nil {
		return "", err
	}

	content := strings.TrimSpace(string(data))

	format, encrypted := encryption.DetectFormat([]byte(content), f.path)
	if !encrypted {
		return content, nil
	}

	decrypted, err := encryption.DecryptContent([]byte(content), format)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt %s_FILE content: %w", f.envName, err)
	}

	return strings.TrimSpace(string(decrypted)), nil
}

// failLocked logs a failure once, not on every use.
func (f *ReloadingFile) failLocked(what string, err error) {
	key := what
	if err != nil {
		key += ": " + err.Error()
	}

	if key == f.failure {
		return
	}

	f.failure = key
	f.info = nil // force a re-read on the next use, whatever the stamp of the next file

	attrs := []any{
		slog.String("path", f.path),
		slog.Duration("value_age", time.Since(f.loadedAt).Round(time.Second)),
	}
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}

	f.log.Warn(f.envName+"_FILE "+what+", keeping the last good value", attrs...)
}

// recoveredLocked logs once that the file is usable again after a failure.
func (f *ReloadingFile) recoveredLocked() {
	if f.failure == "" {
		return
	}

	prev := f.failure
	f.failure = ""

	f.log.Info(f.envName+"_FILE usable again", slog.String("path", f.path), slog.String("previous", prev))
}
