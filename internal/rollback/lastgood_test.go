package rollback

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestLastGoodRoundTrip(t *testing.T) {
	data := t.TempDir()
	s := StoreFor(data)

	if _, ok, err := s.GetLastGood("default/web"); ok || err != nil {
		t.Fatalf("missing last good: ok=%v err=%v", ok, err)
	}

	lg := LastGood{
		Version: 42, Object: "default/web", CommitSHA: "abc", SourceURL: "https://example.com/r.git",
		ProjectHash: "h1", DeployConfig: []byte("name: web\n"),
		Images: []ServiceImage{
			{Service: "app", Image: "nginx:1", ImageID: "sha256:1", RepoDigests: []string{"nginx@sha256:d"}},
			{Service: "db", Image: "postgres:16", ImageID: "sha256:2"},
		},
		RecordedAt: time.Unix(20, 0).UTC(),
	}
	if err := s.PutLastGood(lg); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(data, "fork", "lastgood", "default", "web.json")

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected %s: %v", path, err)
	}

	var onDisk map[string]any
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}

	if onDisk["version"] != float64(1) {
		t.Fatalf("version on disk = %v, want 1", onDisk["version"])
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("last good file must be private, got %v", perm)
	}

	got, ok, err := s.GetLastGood("default/web")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}

	lg.Version = 1
	if !reflect.DeepEqual(got, lg) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, lg)
	}
}

func TestLastGoodEscapesObject(t *testing.T) {
	data := t.TempDir()
	s := StoreFor(data)

	if err := s.PutLastGood(LastGood{Object: "noi/poli gate"}); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(data, "fork", "lastgood", "noi", "poli%20gate.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s: %v", path, err)
	}
}

func TestLastGoodCorruptFile(t *testing.T) {
	data := t.TempDir()
	s := StoreFor(data)
	path := filepath.Join(data, "fork", "lastgood", "default", "x.json")
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = os.WriteFile(path, []byte(`{"version":1,"object":`), 0o600)

	if _, ok, err := s.GetLastGood("default/x"); err == nil || ok {
		t.Fatalf("expected error on corrupt file, got ok=%v err=%v", ok, err)
	}

	_ = os.WriteFile(path, []byte(`{"version":99}`), 0o600)

	if _, ok, err := s.GetLastGood("default/x"); err == nil || ok {
		t.Fatalf("expected error on unknown version, got ok=%v err=%v", ok, err)
	}
}

func TestLastGoodBadObject(t *testing.T) {
	s := StoreFor(t.TempDir())
	for _, o := range []string{"", "noslash", "/x", "x/", "a/b/c", "../x", "x/..", "./x"} {
		if err := s.PutLastGood(LastGood{Object: o}); err == nil {
			t.Errorf("put: object %q must be rejected", o)
		}

		if _, _, err := s.GetLastGood(o); err == nil {
			t.Errorf("get: object %q must be rejected", o)
		}
	}
}

// Year outside [0, 9999]: time.Time does not serialize to JSON.
func TestLastGoodPutEncodeError(t *testing.T) {
	s := StoreFor(t.TempDir())

	err := s.PutLastGood(LastGood{Object: "default/x", RecordedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err == nil {
		t.Fatal("expected encode error")
	}
}

// If <data>/fork is a file, the directory cannot be created.
func TestLastGoodPutWriteError(t *testing.T) {
	data := t.TempDir()
	if err := os.WriteFile(filepath.Join(data, "fork"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := StoreFor(data).PutLastGood(LastGood{Object: "default/x"}); err == nil {
		t.Fatal("expected write error when root is a file")
	}
}

// A read error other than not-exist (here: the path is a directory) does not
// mean "no last good".
func TestLastGoodGetReadError(t *testing.T) {
	data := t.TempDir()
	s := StoreFor(data)

	if err := os.MkdirAll(filepath.Join(data, "fork", "lastgood", "default", "x.json"), 0o700); err != nil {
		t.Fatal(err)
	}

	if _, ok, err := s.GetLastGood("default/x"); err == nil || ok {
		t.Fatalf("expected read error, got ok=%v err=%v", ok, err)
	}
}
