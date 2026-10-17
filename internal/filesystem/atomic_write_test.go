package filesystem

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// swap replaces *target with value for the duration of the test and restores the original.
func swap[T any](t *testing.T, target *T, value T) {
	t.Helper()

	old := *target
	*target = value

	t.Cleanup(func() { *target = old })
}

// failingTemp is a real temporary file where a single operation can fail.
type failingTemp struct {
	*os.File
	writeErr, chmodErr, syncErr, closeErr error
}

func (f *failingTemp) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}

	return f.File.Write(p)
}

func (f *failingTemp) Chmod(mode os.FileMode) error {
	if f.chmodErr != nil {
		return f.chmodErr
	}

	return f.File.Chmod(mode)
}

func (f *failingTemp) Sync() error {
	if f.syncErr != nil {
		return f.syncErr
	}

	return f.File.Sync()
}

func (f *failingTemp) Close() error {
	// Always close the real file, so the test leaves no open descriptors behind.
	err := f.File.Close()
	if f.closeErr != nil {
		return f.closeErr
	}

	return err
}

// failingDir is a real directory where Sync can fail.
type failingDir struct {
	*os.File
	syncErr error
}

func (d *failingDir) Sync() error {
	if d.syncErr != nil {
		return d.syncErr
	}

	return d.File.Sync()
}

// injectTempFile makes AtomicWriteFile create a real temporary file altered by mutate.
func injectTempFile(t *testing.T, mutate func(*failingTemp)) {
	t.Helper()

	swap(t, &createTempFile, func(dir, pattern string) (tempFile, error) {
		f, err := os.CreateTemp(dir, pattern)
		if err != nil {
			return nil, err
		}

		ft := &failingTemp{File: f}
		mutate(ft)

		return ft, nil
	})
}

func listNames(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}

	return names
}

func requireNoTempFiles(t *testing.T, dir string) {
	t.Helper()

	for _, name := range listNames(t, dir) {
		if strings.HasPrefix(name, ".tmp-") {
			t.Fatalf("temp file left behind: %s", name)
		}
	}
}

func TestAtomicWriteFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	path := filepath.Join(dir, "x.json")

	if err := AtomicWriteFile(path, []byte(`{"v":1}`), PermPrivateFile); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil || string(got) != `{"v":1}` {
		t.Fatalf("got %q, %v", got, err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Perm() != PermPrivateFile {
		t.Fatalf("perm %v", info.Mode().Perm())
	}

	dinfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}

	if dinfo.Mode().Perm() != PermPrivateDir {
		t.Fatalf("dir perm %v", dinfo.Mode().Perm())
	}

	if entries := listNames(t, dir); len(entries) != 1 {
		t.Fatalf("temp file left behind: %v", entries)
	}
}

func TestAtomicWriteFileErrors(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := AtomicWriteFile(filepath.Join(blocker, "x"), []byte("x"), PermPrivateFile); err == nil {
		t.Fatal("expected error when parent is a file")
	}
}

func TestAtomicWriteFileRenameOntoDirectory(t *testing.T) {
	// rename(2) of a file onto a non-empty directory fails with EISDIR.
	path := filepath.Join(t.TempDir(), "x")
	if err := os.Mkdir(path, PermPrivateDir); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(path, "inner"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := AtomicWriteFile(path, []byte("x"), PermPrivateFile); err == nil {
		t.Fatal("expected error when target is a non-empty directory")
	}

	requireNoTempFiles(t, filepath.Dir(path))
}

func TestAtomicWriteFileCreateTempReadOnlyDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions: CreateTemp does not fail")
	}

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}

	// t.TempDir cleans up after this Cleanup (LIFO): the directory must be writable again.
	t.Cleanup(func() { _ = os.Chmod(dir, PermPrivateDir) })

	if err := AtomicWriteFile(filepath.Join(dir, "x"), []byte("x"), PermPrivateFile); err == nil {
		t.Fatal("expected error when directory is read-only")
	}
}

func TestAtomicWriteFileCreateTempError(t *testing.T) {
	swap(t, &createTempFile, func(string, string) (tempFile, error) {
		return nil, errors.New("injected create failure")
	})

	dir := t.TempDir()
	if err := AtomicWriteFile(filepath.Join(dir, "x"), []byte("x"), PermPrivateFile); err == nil {
		t.Fatal("expected error when temp file cannot be created")
	}
}

func TestAtomicWriteFileTempOperationErrors(t *testing.T) {
	injected := errors.New("injected failure")

	cases := map[string]func(*failingTemp){
		"write": func(f *failingTemp) { f.writeErr = injected },
		"chmod": func(f *failingTemp) { f.chmodErr = injected },
		"sync":  func(f *failingTemp) { f.syncErr = injected },
		"close": func(f *failingTemp) { f.closeErr = injected },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			injectTempFile(t, mutate)

			dir := t.TempDir()
			if err := AtomicWriteFile(filepath.Join(dir, "x"), []byte("x"), PermPrivateFile); !errors.Is(err, injected) {
				t.Fatalf("expected injected %s error, got %v", name, err)
			}

			requireNoTempFiles(t, dir)
		})
	}
}

func TestAtomicWriteFileRenameError(t *testing.T) {
	swap(t, &renameFile, func(string, string) error { return errors.New("injected rename failure") })

	dir := t.TempDir()
	if err := AtomicWriteFile(filepath.Join(dir, "x"), []byte("x"), PermPrivateFile); err == nil {
		t.Fatal("expected error when rename fails")
	}

	requireNoTempFiles(t, dir)
}

func TestAtomicWriteFileOpenDirectoryError(t *testing.T) {
	swap(t, &openDirectory, func(string) (dirFile, error) {
		return nil, errors.New("injected open failure")
	})

	dir := t.TempDir()
	if err := AtomicWriteFile(filepath.Join(dir, "x"), []byte("x"), PermPrivateFile); err == nil {
		t.Fatal("expected error when directory cannot be opened")
	}
}

func TestAtomicWriteFileDirectorySyncError(t *testing.T) {
	swap(t, &openDirectory, func(name string) (dirFile, error) {
		d, err := os.Open(name)
		if err != nil {
			return nil, err
		}

		return &failingDir{File: d, syncErr: errors.New("injected dir sync failure")}, nil
	})

	dir := t.TempDir()
	if err := AtomicWriteFile(filepath.Join(dir, "x"), []byte("x"), PermPrivateFile); err == nil {
		t.Fatal("expected error when directory sync fails")
	}
}
