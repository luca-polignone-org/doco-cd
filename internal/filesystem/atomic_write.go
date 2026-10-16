package filesystem

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	PermPrivateDir  = 0o700 // directory leggibile solo dal processo
	PermPrivateFile = 0o600 // file leggibile solo dal processo
)

// AtomicWriteFile scrive data in path passando da un file temporaneo nella stessa
// directory: write, fsync, rename, fsync della directory. Un crash a metà lascia il
// file precedente intatto, mai uno scritto a metà. È lo schema di selfupdate.Store.
func AtomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, PermPrivateDir); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}

	file, err := os.CreateTemp(dir, ".tmp-*")
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

	if err = os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("publish %s: %w", path, err)
	}

	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open directory %s: %w", dir, err)
	}
	defer d.Close() // nolint:errcheck

	if err = d.Sync(); err != nil {
		return fmt.Errorf("sync directory %s: %w", dir, err)
	}

	return nil
}
