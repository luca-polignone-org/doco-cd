package filesystem

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	PermPrivateDir  = 0o700 // directory readable only by the process
	PermPrivateFile = 0o600 // file readable only by the process
)

// tempFile is the subset of *os.File used by AtomicWriteFile; an interface
// lets tests inject errors into individual operations.
type tempFile interface {
	Name() string
	Write(p []byte) (int, error)
	Chmod(mode os.FileMode) error
	Sync() error
	Close() error
}

// dirFile is the subset of *os.File used to fsync the directory.
type dirFile interface {
	Sync() error
	Close() error
}

// Test seams: in production they point to the os functions, tests replace them
// to inject errors the real filesystem does not produce deterministically.
var (
	createTempFile = func(dir, pattern string) (tempFile, error) {
		f, err := os.CreateTemp(dir, pattern)
		if err != nil {
			return nil, err
		}

		return f, nil
	}
	renameFile    = os.Rename
	openDirectory = func(name string) (dirFile, error) {
		d, err := os.Open(name)
		if err != nil {
			return nil, err
		}

		return d, nil
	}
)

// AtomicWriteFile writes data to path via a temporary file in the same
// directory: write, fsync, rename, fsync of the directory. A crash midway leaves the
// previous file intact, never a partially written one. Same scheme as selfupdate.Store.
func AtomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, PermPrivateDir); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}

	file, err := createTempFile(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file in %s: %w", dir, err)
	}

	tempPath := file.Name()
	defer func() { _ = os.Remove(tempPath) }()

	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}

	if err = file.Chmod(perm); err != nil {
		_ = file.Close()
		return fmt.Errorf("chmod %s: %w", path, err)
	}

	if err = file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync %s: %w", path, err)
	}

	if err = file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}

	if err = renameFile(tempPath, path); err != nil {
		return fmt.Errorf("publish %s: %w", path, err)
	}

	d, err := openDirectory(dir)
	if err != nil {
		return fmt.Errorf("open directory %s: %w", dir, err)
	}
	defer d.Close() // nolint:errcheck

	if err = d.Sync(); err != nil {
		return fmt.Errorf("sync directory %s: %w", dir, err)
	}

	return nil
}
