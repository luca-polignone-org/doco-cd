package filesystem

import (
	"os"
	"path/filepath"
	"testing"
)

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

	info, _ := os.Stat(path)
	if info.Mode().Perm() != PermPrivateFile {
		t.Fatalf("perm %v", info.Mode().Perm())
	}

	dinfo, _ := os.Stat(dir)
	if dinfo.Mode().Perm() != PermPrivateDir {
		t.Fatalf("dir perm %v", dinfo.Mode().Perm())
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
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
