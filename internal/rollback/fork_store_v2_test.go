package rollback

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreReadsVersion1(t *testing.T) {
	data := t.TempDir()
	s := StoreFor(data)

	path := filepath.Join(data, "fork", "failures", "default", "web.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	v1 := `{"version":1,"object":"default/web","commit_sha":"c1","stage":"deploy","state":"rolled_back"}`
	if err := os.WriteFile(path, []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}

	f, ok, err := s.GetFailure("default/web")
	if err != nil || !ok || f.State != StateRolledBack || f.CommitSHA != "c1" || f.ContentKey != "" {
		t.Fatalf("GetFailure() = %+v, %v, %v; want the v1 record unchanged", f, ok, err)
	}
}

func TestStoreWritesVersion2WithContentKey(t *testing.T) {
	s := StoreFor(t.TempDir())

	if err := s.PutFailure(Failure{Object: "default/web", CommitSHA: "c1", State: StateBlocked, ContentKey: "tree:a;config:b"}); err != nil {
		t.Fatal(err)
	}

	f, ok, err := s.GetFailure("default/web")
	if err != nil || !ok || f.Version != 2 || f.ContentKey != "tree:a;config:b" {
		t.Fatalf("GetFailure() = %+v, %v, %v", f, ok, err)
	}
}

func TestStoreRejectsUnknownVersion(t *testing.T) {
	data := t.TempDir()
	s := StoreFor(data)

	path := filepath.Join(data, "fork", "failures", "default", "web.json")
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = os.WriteFile(path, []byte(`{"version":3,"object":"default/web"}`), 0o600)

	if _, _, err := s.GetFailure("default/web"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("GetFailure() error = %v, want ErrCorrupt", err)
	}
}

func TestLastGoodStaysVersion1(t *testing.T) {
	data := t.TempDir()
	s := StoreFor(data)

	if err := s.PutLastGood(LastGood{Object: "default/web", CommitSHA: "c1"}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(data, "fork", "lastgood", "default", "web.json"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(raw), `"version":1`) {
		t.Fatalf("last good record = %s, want version 1", raw)
	}

	if _, ok, err := s.GetLastGood("default/web"); err != nil || !ok {
		t.Fatalf("GetLastGood() = %v, %v", ok, err)
	}
}
