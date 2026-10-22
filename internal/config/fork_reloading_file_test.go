package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/encryption"
)

const keepMsg = "keeping the last good value"

func newForkTestLog() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer

	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// replaceAtomically writes content next to path and renames it over path, as the Portico master does.
func replaceAtomically(t *testing.T, path, content string) {
	t.Helper()

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil { // #nosec G703 -- path is constructed from t.TempDir(), not user input
		t.Fatal(err)
	}

	if err := os.Rename(tmp, path); err != nil { // #nosec G703 -- path is constructed from t.TempDir(), not user input
		t.Fatal(err)
	}
}

func newForkTokenFile(t *testing.T, content string) (string, *ReloadingFile, *bytes.Buffer) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "git_access_token")
	replaceAtomically(t, path, content)

	log, buf := newForkTestLog()

	f, err := NewReloadingFile("GIT_ACCESS_TOKEN", path, log)
	if err != nil {
		t.Fatalf("NewReloadingFile() error = %v", err)
	}

	return path, f, buf
}

func wantValue(t *testing.T, f *ReloadingFile, want string) {
	t.Helper()

	if got := f.Value(); got != want {
		t.Fatalf("Value() = %q, want %q", got, want)
	}
}

func TestForkReloadingFile_InitialValueTrimmed(t *testing.T) {
	_, f, _ := newForkTokenFile(t, "  tok-one\n")
	wantValue(t, f, "tok-one")
}

func TestForkReloadingFile_MissingAtStartupFails(t *testing.T) {
	log, _ := newForkTestLog()

	if _, err := NewReloadingFile("GIT_ACCESS_TOKEN", filepath.Join(t.TempDir(), "nope"), log); err == nil {
		t.Fatal("NewReloadingFile() error = nil, want an error for a missing file")
	}
}

func TestForkReloadingFile_UnreadableAtStartupFails(t *testing.T) {
	log, _ := newForkTestLog()

	if _, err := NewReloadingFile("GIT_ACCESS_TOKEN", t.TempDir(), log); err == nil {
		t.Fatal("NewReloadingFile() error = nil, want an error for a directory")
	}
}

func TestForkReloadingFile_NilLoggerUsesDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "git_access_token")
	replaceAtomically(t, path, "tok-one")

	f, err := NewReloadingFile("GIT_ACCESS_TOKEN", path, nil)
	if err != nil {
		t.Fatalf("NewReloadingFile() error = %v", err)
	}

	wantValue(t, f, "tok-one")
}

func TestForkReloadingFile_EmptyAtStartupFilledLater(t *testing.T) {
	path, f, _ := newForkTokenFile(t, "")
	wantValue(t, f, "")

	replaceAtomically(t, path, "tok-one")
	wantValue(t, f, "tok-one")
}

// A GitHub token has a fixed length and the rewrite may land in the same mtime tick: the new inode
// alone must trigger the reload.
func TestForkReloadingFile_RenameWithSameSizeAndMtime(t *testing.T) {
	path, f, buf := newForkTokenFile(t, "tok-aaaa")

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	replaceAtomically(t, path, "tok-bbbb")

	if err = os.Chtimes(path, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}

	wantValue(t, f, "tok-bbbb")

	if !strings.Contains(buf.String(), "GIT_ACCESS_TOKEN_FILE reloaded") {
		t.Fatalf("log = %q, want a reload line", buf.String())
	}
}

func TestForkReloadingFile_InPlaceRewriteWithNewMtime(t *testing.T) {
	path, f, _ := newForkTokenFile(t, "tok-aaaa")

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if err = os.WriteFile(path, []byte("tok-bbbb"), 0o600); err != nil { // #nosec G703 -- path is constructed from t.TempDir(), not user input
		t.Fatal(err)
	}

	next := st.ModTime().Add(time.Second)
	if err = os.Chtimes(path, next, next); err != nil {
		t.Fatal(err)
	}

	wantValue(t, f, "tok-bbbb")
}

// Contract: an in-place write that keeps inode, size and mtime is not seen. The master renames.
func TestForkReloadingFile_UnchangedStampIsNotReread(t *testing.T) {
	path, f, _ := newForkTokenFile(t, "tok-aaaa")

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if err = os.WriteFile(path, []byte("tok-bbbb"), 0o600); err != nil { // #nosec G703 -- path is constructed from t.TempDir(), not user input
		t.Fatal(err)
	}

	if err = os.Chtimes(path, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}

	wantValue(t, f, "tok-aaaa")
}

func TestForkReloadingFile_MissingKeepsValueAndWarnsOnce(t *testing.T) {
	path, f, buf := newForkTokenFile(t, "tok-one")

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	wantValue(t, f, "tok-one")
	wantValue(t, f, "tok-one")

	if n := strings.Count(buf.String(), keepMsg); n != 1 {
		t.Fatalf("%d warnings, want 1: %q", n, buf.String())
	}

	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "value_age=") {
		t.Fatalf("log = %q, want a WARN with value_age", buf.String())
	}

	replaceAtomically(t, path, "tok-two")
	wantValue(t, f, "tok-two")
}

func TestForkReloadingFile_SameFileBackLogsRecovery(t *testing.T) {
	path, f, buf := newForkTokenFile(t, "tok-one")

	if err := os.Rename(path, path+".away"); err != nil {
		t.Fatal(err)
	}

	wantValue(t, f, "tok-one")

	if err := os.Rename(path+".away", path); err != nil {
		t.Fatal(err)
	}

	wantValue(t, f, "tok-one")

	if !strings.Contains(buf.String(), "GIT_ACCESS_TOKEN_FILE usable again") {
		t.Fatalf("log = %q, want a recovery line", buf.String())
	}
}

func TestForkReloadingFile_EmptyKeepsValue(t *testing.T) {
	path, f, buf := newForkTokenFile(t, "tok-one")

	replaceAtomically(t, path, "\n")
	wantValue(t, f, "tok-one")
	wantValue(t, f, "tok-one")

	if !strings.Contains(buf.String(), "GIT_ACCESS_TOKEN_FILE empty, "+keepMsg) {
		t.Fatalf("log = %q, want the empty warning", buf.String())
	}

	if n := strings.Count(buf.String(), keepMsg); n != 1 {
		t.Fatalf("%d warnings, want 1", n)
	}

	replaceAtomically(t, path, "tok-two")
	wantValue(t, f, "tok-two")

	if !strings.Contains(buf.String(), "GIT_ACCESS_TOKEN_FILE usable again") {
		t.Fatalf("log = %q, want a recovery line", buf.String())
	}
}

func TestForkReloadingFile_UnreadableKeepsValue(t *testing.T) {
	path, f, buf := newForkTokenFile(t, "tok-one")

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}

	wantValue(t, f, "tok-one")

	if !strings.Contains(buf.String(), "GIT_ACCESS_TOKEN_FILE unreadable, "+keepMsg) {
		t.Fatalf("log = %q, want the unreadable warning", buf.String())
	}
}

// A file that looks SOPS-encrypted but cannot be decrypted keeps the value, as unreadable.
func TestForkReloadingFile_UndecryptableKeepsValue(t *testing.T) {
	path, f, buf := newForkTokenFile(t, "tok-one")

	sops := `{"data":"ENC[AES256_GCM,data:AAAA,iv:AAAA,tag:AAAA,type:str]","sops":{"kms":null,"gcp_kms":null,"azure_kv":null,"hc_vault":null,"age":[{"recipient":"age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq","enc":"x"}],"lastmodified":"2026-10-22T00:00:00Z","mac":"ENC[AES256_GCM,data:AAAA,iv:AAAA,tag:AAAA,type:str]","pgp":null,"unencrypted_suffix":"_unencrypted","version":"3.9.0"}}`
	replaceAtomically(t, path, sops)

	wantValue(t, f, "tok-one")

	if !strings.Contains(buf.String(), "GIT_ACCESS_TOKEN_FILE unreadable, "+keepMsg) {
		t.Fatalf("log = %q, want the unreadable warning", buf.String())
	}
}

func TestForkReloadingFile_NeverLogsTheValue(t *testing.T) {
	path, f, buf := newForkTokenFile(t, "secret-token-1")

	replaceAtomically(t, path, "secret-token-2")
	f.Value()
	replaceAtomically(t, path, "")
	f.Value()

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	f.Value()
	replaceAtomically(t, path, "secret-token-3")
	f.Value()

	if strings.Contains(buf.String(), "secret-token") {
		t.Fatalf("log carries a token: %q", buf.String())
	}
}

func TestForkReloadingFile_ConcurrentReadersAndWriter(t *testing.T) {
	path, f, _ := newForkTokenFile(t, "tok-000")

	written := map[string]bool{"tok-000": true}
	for i := 1; i <= 50; i++ {
		written[fmt.Sprintf("tok-%03d", i)] = true
	}

	var wg sync.WaitGroup

	errs := make(chan string, 8*200)

	for range 8 {
		wg.Go(func() {
			for range 200 {
				if v := f.Value(); !written[v] {
					errs <- v
				}
			}
		})
	}

	for i := 1; i <= 50; i++ {
		replaceAtomically(t, path, fmt.Sprintf("tok-%03d", i))
	}

	wg.Wait()
	close(errs)

	for v := range errs {
		t.Fatalf("Value() = %q, never written", v)
	}

	wantValue(t, f, "tok-050")
}

// The upstream SOPS fixture (age, test key) decrypts to "binary-secret": the token file may be encrypted.
func TestForkReloadingFile_DecryptsSopsContent(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	data, err := os.ReadFile(filepath.Join("..", "encryption", "testdata", "encrypted"))
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "git_access_token")
	replaceAtomically(t, path, string(data))

	log, _ := newForkTestLog()

	f, err := NewReloadingFile("GIT_ACCESS_TOKEN", path, log)
	if err != nil {
		t.Fatalf("NewReloadingFile() error = %v", err)
	}

	wantValue(t, f, "binary-secret")
}
