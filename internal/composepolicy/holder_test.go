package composepolicy

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const holderValid = "version: 1\ntargets: {\"noi/a\": {repository: https://x/y}}\n"

func writeAt(t *testing.T, path, content string, mtime time.Time) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestHolderReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	base := time.Now().Add(-time.Hour)
	writeAt(t, path, holderValid, base)

	var logs bytes.Buffer

	h, err := NewHolder(path, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}

	first := h.Current()
	if first == nil || first.Targets["noi/a"].Repository != "https://x/y" {
		t.Fatalf("unexpected current: %+v", first)
	}

	if h.Current() != first {
		t.Error("unchanged file must not be reloaded")
	}

	// File invalido: si tiene la versione vecchia e si logga una volta sola.
	writeAt(t, path, "version: 2\n", base.Add(time.Second))

	if h.Current() != first {
		t.Fatal("invalid file replaced the last valid policy")
	}

	if n := strings.Count(logs.String(), "compose policy reload failed"); n != 1 {
		t.Fatalf("expected 1 error log, got %d", n)
	}

	if h.Current() != first {
		t.Fatal("invalid file replaced the last valid policy on second call")
	}

	if n := strings.Count(logs.String(), "compose policy reload failed"); n != 1 {
		t.Fatalf("failed version logged again: %d lines", n)
	}

	// File valido di nuovo: nuova versione.
	writeAt(t, path, "version: 1\ntargets: {\"noi/b\": {repository: https://x/z}}\n", base.Add(2*time.Second))

	next := h.Current()
	if next == first || next.Targets["noi/b"].Repository != "https://x/z" {
		t.Fatalf("expected new policy, got %+v", next)
	}

	// File sparito: si tiene l'ultima versione valida.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	if h.Current() != next {
		t.Error("missing file must keep the last valid policy")
	}
}

func TestHolderErrors(t *testing.T) {
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	dir := t.TempDir()

	if _, err := NewHolder(filepath.Join(dir, "missing.yaml"), log); err == nil {
		t.Error("missing file: expected error")
	}

	bad := filepath.Join(dir, "bad.yaml")
	writeAt(t, bad, "version: [", time.Now())

	if _, err := NewHolder(bad, log); err == nil {
		t.Error("invalid file: expected error")
	}

	// Una directory supera la stat ma non la lettura.
	if _, err := NewHolder(dir, log); err == nil {
		t.Error("directory: expected error")
	}
}

func TestHolderNil(t *testing.T) {
	var h *Holder
	if h.Current() != nil {
		t.Error("nil holder must return a nil policy")
	}
}
