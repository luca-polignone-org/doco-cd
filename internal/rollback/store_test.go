package rollback

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreRoundTrip(t *testing.T) {
	data := t.TempDir()

	s := StoreFor(data)
	if s != StoreFor(data) {
		t.Fatal("StoreFor must return the shared instance")
	}

	if s.Root() != filepath.Join(data, "fork") {
		t.Fatalf("unexpected root %q", s.Root())
	}

	f := Failure{
		Object: "noi/poli gate", CommitSHA: "abc", Stage: "deploy", Error: "boom",
		FailedAt: time.Unix(10, 0).UTC(), State: StateRolledBack,
	}
	if err := s.PutFailure(f); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(data, "fork", "failures", "noi", "poli%20gate.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s: %v", path, err)
	}

	got, ok, err := s.GetFailure("noi/poli gate")
	if err != nil || !ok || got.State != StateRolledBack || got.Version != 2 || got.CommitSHA != "abc" {
		t.Fatalf("%+v %v %v", got, ok, err)
	}

	if !s.Blocks(got) || s.Blocks(Failure{State: StateFailed}) {
		t.Fatal("Blocks")
	}

	if err := s.DeleteFailure("noi/poli gate"); err != nil {
		t.Fatal(err)
	}

	if _, ok, _ := s.GetFailure("noi/poli gate"); ok {
		t.Fatal("deleted failure still present")
	}

	if err := s.DeleteFailure("noi/poli gate"); err != nil {
		t.Fatal("delete of missing must be nil")
	}
}

// An unreadable file is not "no failure": it is an error, which the caller
// treats as blocked (spec, "Store").
func TestStoreCorruptFile(t *testing.T) {
	data := t.TempDir()
	s := StoreFor(data)
	path := filepath.Join(data, "fork", "failures", "default", "x.json")
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = os.WriteFile(path, []byte(`{"version":1,"state":`), 0o600)

	if _, _, err := s.GetFailure("default/x"); err == nil {
		t.Fatal("expected error on corrupt file")
	}

	_ = os.WriteFile(path, []byte(`{"version":99}`), 0o600)

	if _, _, err := s.GetFailure("default/x"); err == nil {
		t.Fatal("expected error on unknown version")
	}
}

func TestStoreBadObject(t *testing.T) {
	s := StoreFor(t.TempDir())
	for _, o := range []string{"", "noslash", "/x", "x/", "a/b/c", "../x", "x/..", "./x"} {
		if err := s.PutFailure(Failure{Object: o}); err == nil {
			t.Errorf("put: object %q must be rejected", o)
		}

		if _, _, err := s.GetFailure(o); err == nil {
			t.Errorf("get: object %q must be rejected", o)
		}

		if err := s.DeleteFailure(o); err == nil {
			t.Errorf("delete: object %q must be rejected", o)
		}
	}
}

// Year outside [0, 9999]: time.Time does not serialize to JSON.
func TestStorePutEncodeError(t *testing.T) {
	s := StoreFor(t.TempDir())

	err := s.PutFailure(Failure{Object: "default/x", FailedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err == nil {
		t.Fatal("expected encode error")
	}
}

// If <data>/fork is a file, the directory cannot be created.
func TestStorePutWriteError(t *testing.T) {
	data := t.TempDir()
	if err := os.WriteFile(filepath.Join(data, "fork"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := StoreFor(data).PutFailure(Failure{Object: "default/x"}); err == nil {
		t.Fatal("expected write error when root is a file")
	}
}

// A read error other than not-exist (here: the path is a directory) does not
// mean "no failure".
func TestStoreGetReadError(t *testing.T) {
	data := t.TempDir()
	s := StoreFor(data)

	if err := os.MkdirAll(filepath.Join(data, "fork", "failures", "default", "x.json"), 0o700); err != nil {
		t.Fatal(err)
	}

	if _, ok, err := s.GetFailure("default/x"); err == nil || ok {
		t.Fatalf("expected read error, got ok=%v err=%v", ok, err)
	}
}

// A non-empty directory in place of the file makes os.Remove fail with an error
// other than not-exist.
func TestStoreDeleteError(t *testing.T) {
	data := t.TempDir()
	s := StoreFor(data)

	dir := filepath.Join(data, "fork", "failures", "default", "x.json")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "child"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteFailure("default/x"); err == nil {
		t.Fatal("expected delete error")
	}
}
