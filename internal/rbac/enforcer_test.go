package rbac

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const enforcerPolicy = "p, alice, projects, get, noi/*, allow\np, alice, projects, sync, noi/*, allow\n"

func writePolicy(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newTestEnforcer(t *testing.T, content string) (*Enforcer, string, *bytes.Buffer) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "policy.csv")
	writePolicy(t, path, content)

	buf := &bytes.Buffer{}

	e, err := NewFileEnforcer(path, slog.New(slog.NewJSONHandler(buf, nil)))
	if err != nil {
		t.Fatal(err)
	}

	return e, path, buf
}

func TestEnforce(t *testing.T) {
	e, _, buf := newTestEnforcer(t, enforcerPolicy)
	alice := Principal{Subject: "alice", Groups: []string{"g1"}}

	if !e.Enabled() {
		t.Fatal("enforcer must be enabled")
	}

	if !e.Enforce(alice, "projects", "get", "noi/web") {
		t.Fatal("get must be allowed")
	}

	if buf.Len() != 0 {
		t.Fatalf("allowed get must not be audited, got %s", buf.String())
	}

	if !e.Enforce(alice, "projects", "sync", "noi/web") {
		t.Fatal("sync must be allowed")
	}

	if e.Enforce(alice, "projects", "delete", "noi/web") {
		t.Fatal("delete must be denied")
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 audit lines, got %d: %s", len(lines), buf.String())
	}

	wantDecisions := []string{"allow", "deny"}

	for i, l := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatal(err)
		}

		if rec["decision"] != wantDecisions[i] {
			t.Errorf("line %d decision = %v, want %s", i, rec["decision"], wantDecisions[i])
		}

		for _, k := range []string{"sub", "groups", "resource", "action", "object", "decision"} {
			delete(rec, k)
		}

		for _, k := range []string{"time", "level", "msg"} {
			delete(rec, k)
		}

		if len(rec) != 0 {
			t.Errorf("line %d has unexpected audit fields: %v", i, rec)
		}
	}

	if !strings.Contains(lines[1], `"decision":"deny"`) {
		t.Errorf("deny line missing decision: %s", lines[1])
	}
}

func TestEnforceNil(t *testing.T) {
	var e *Enforcer

	if e.Enabled() {
		t.Fatal("nil enforcer must be disabled")
	}

	if !e.Enforce(Principal{}, "projects", "delete", "x/y") {
		t.Fatal("nil enforcer must allow everything")
	}
}

func TestReloadOnChange(t *testing.T) {
	e, path, _ := newTestEnforcer(t, enforcerPolicy)
	bob := Principal{Subject: "bob"}

	if e.Enforce(bob, "projects", "get", "noi/web") {
		t.Fatal("bob must start denied")
	}

	writePolicy(t, path, enforcerPolicy+"p, bob, projects, get, noi/*, allow\n")

	if !e.Enforce(bob, "projects", "get", "noi/web") {
		t.Fatal("bob must be allowed after reload")
	}
}

func TestReloadKeepsLastValidPolicy(t *testing.T) {
	e, path, buf := newTestEnforcer(t, enforcerPolicy)
	alice := Principal{Subject: "alice"}

	// Truncated mid-line with a different mtime and size: the previous policy stays.
	writePolicy(t, path, "p, alice, projects, get, noi/*")

	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	if !e.Enforce(alice, "projects", "get", "noi/web") {
		t.Fatal("decision must not change after a failed reload")
	}

	if !strings.Contains(buf.String(), "reload failed") {
		t.Fatalf("expected reload failed log, got %q", buf.String())
	}

	// The same broken version is not retried.
	buf.Reset()

	if !e.Enforce(alice, "projects", "get", "noi/web") || buf.Len() != 0 {
		t.Fatalf("broken version must not be retried, log %q", buf.String())
	}

	// A new valid version is picked up again.
	writePolicy(t, path, "p, alice, projects, get, other/*, allow\n")

	if !e.Enforce(alice, "projects", "get", "other/web") {
		t.Fatal("valid version must be reloaded")
	}
}

func TestReloadFileRemoved(t *testing.T) {
	e, path, _ := newTestEnforcer(t, enforcerPolicy)

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	if !e.Enforce(Principal{Subject: "alice"}, "projects", "get", "noi/web") {
		t.Fatal("decision must not change when the file disappears")
	}
}

func TestNewFileEnforcerErrors(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	dir := t.TempDir()

	bad := filepath.Join(dir, "bad.csv")
	writePolicy(t, bad, "x, a, b\n")

	// A directory passes os.Stat but fails os.ReadFile.
	for name, path := range map[string]string{
		"missing":   filepath.Join(dir, "missing.csv"),
		"invalid":   bad,
		"directory": dir,
	} {
		if _, err := NewFileEnforcer(path, log); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
