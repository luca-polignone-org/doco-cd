package openbao

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// kvServer imitates GET /v1/<engine>/data/<name> of KV v2: it answers 200 to the current token
// and 403 to any other one, and counts the requests.
type kvServer struct {
	mu       sync.Mutex
	token    string
	requests atomic.Int32
	issues   atomic.Int32
}

func (s *kvServer) setToken(tok string) { s.mu.Lock(); s.token = tok; s.mu.Unlock() }

func (s *kvServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.requests.Add(1)

	s.mu.Lock()
	ok := r.Header.Get("X-Vault-Token") == s.token
	s.mu.Unlock()

	if strings.Contains(r.URL.Path, "/issue/") {
		s.issues.Add(1)
	}

	if !ok {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":["permission denied"]}`))

		return
	}

	_, _ = w.Write([]byte(`{"data":{"data":{"PASS":"v"},"metadata":{"version":1}}}`))
}

func writeToken(t *testing.T, path, tok string) {
	t.Helper()

	// The agent writes the sink with a rename: so does the test, so a reader never sees half a file.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}

	// mtime granularity: make sure the stamp changes even on fast filesystems.
	future := time.Now().Add(time.Duration(len(tok)) * time.Second)
	_ = os.Chtimes(path, future, future)
}

func reloadingProvider(t *testing.T, srv *kvServer, tok string) (*Provider, string) {
	t.Helper()

	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	path := filepath.Join(t.TempDir(), "token")
	writeToken(t, path, tok)
	srv.setToken(tok)

	p, err := NewProvider(context.Background(), ts.URL, tok)
	if err != nil {
		t.Fatal(err)
	}

	if err = p.EnableTokenReload(path); err != nil {
		t.Fatal(err)
	}

	return p, path
}

func TestTokenReloadOnChange(t *testing.T) {
	srv := &kvServer{}
	p, path := reloadingProvider(t, srv, "tok-1")

	writeToken(t, path, "tok-2")
	srv.setToken("tok-2")

	if _, err := p.GetSecret(context.Background(), "kv:secret:prod/web/db:PASS"); err != nil {
		t.Fatalf("GetSecret() after a token change: %v", err)
	}
}

func TestTokenReloadRetriesOnceOn403(t *testing.T) {
	srv := &kvServer{}
	p, path := reloadingProvider(t, srv, "tok-1")

	// The file changes but its stamp does not: same size ("tok-1" and "tok-2" have the same
	// length) and the saved mtime put back. refreshToken sees nothing, the first request gets 403,
	// and only the forced re-read after the 403 finds the new token: exactly two requests.
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if err = os.WriteFile(path, []byte("tok-2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err = os.Chtimes(path, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}

	srv.setToken("tok-2")

	before := srv.requests.Load()

	if _, err = p.GetSecret(context.Background(), "kv:secret:prod/web/db:PASS"); err != nil {
		t.Fatalf("GetSecret(): %v", err)
	}

	if n := srv.requests.Load() - before; n != 2 {
		t.Fatalf("%d requests, want exactly 2 (a 403, then one retry)", n)
	}
}

func TestTokenReloadNoRetryOnIssue(t *testing.T) {
	srv := &kvServer{}
	p, _ := reloadingProvider(t, srv, "tok-1")
	srv.setToken("other")

	if _, err := p.GetSecret(context.Background(), "pki-role:pki:web-role:web.example.com"); err == nil {
		t.Fatal("GetSecret() on pki-role with a refused token must fail")
	}

	if n := srv.issues.Load(); n != 1 {
		t.Fatalf("%d issue requests, want exactly 1: issuance is never retried", n)
	}
}

func TestTokenReloadEmptyFileKeepsToken(t *testing.T) {
	srv := &kvServer{}
	p, path := reloadingProvider(t, srv, "tok-1")

	writeToken(t, path, "")

	if _, err := p.GetSecret(context.Background(), "kv:secret:prod/web/db:PASS"); err != nil {
		t.Fatalf("an empty file must keep the token in use: %v", err)
	}
}

func TestTokenReloadConcurrent(t *testing.T) {
	srv := &kvServer{}
	p, path := reloadingProvider(t, srv, "tok-1")

	var wg sync.WaitGroup

	for i := range 20 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			if i == 10 {
				writeToken(t, path, "tok-2")
				srv.setToken("tok-2")
			}

			_, _ = p.ResolveSecretReferences(context.Background(), map[string]string{"A": "kv:secret:prod/web/db:PASS"})
		}()
	}

	wg.Wait()

	if _, err := p.ResolveSecretReferences(context.Background(), map[string]string{"A": "kv:secret:prod/web/db:PASS"}); err != nil {
		t.Fatalf("after the race the new token must be in use: %v", err)
	}
}

func TestTokenReloadOff(t *testing.T) {
	srv := &kvServer{}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	srv.setToken("tok-1")

	p, err := NewProvider(context.Background(), ts.URL, "tok-1")
	if err != nil {
		t.Fatal(err)
	}

	p.refreshToken() // no path: nothing to do, no panic

	if _, err = p.GetSecret(context.Background(), "kv:secret:prod/web/db:PASS"); err != nil {
		t.Fatal(err)
	}
}

func TestValidateTokenReload(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg     Config
		fromEnv bool
		ok      bool
	}{
		"off":               {Config{}, true, true},
		"on with file":      {Config{AccessTokenFileReload: true, AccessTokenFilePath: "/run/t"}, false, true},
		"on without file":   {Config{AccessTokenFileReload: true}, false, false},
		"on with env token": {Config{AccessTokenFileReload: true, AccessTokenFilePath: "/run/t"}, true, false},
	} {
		t.Run(name, func(t *testing.T) {
			if err := tc.cfg.ValidateTokenReload(tc.fromEnv); (err == nil) != tc.ok {
				t.Fatalf("ValidateTokenReload() error = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

// revokedServer imitates LIST /v1/<engine>/certs/revoked: it answers the serial "0a" to the
// current token and 403 to any other one.
func revokedServer(t *testing.T, srv *kvServer) *httptest.Server {
	t.Helper()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.requests.Add(1)

		srv.mu.Lock()
		ok := r.Header.Get("X-Vault-Token") == srv.token
		srv.mu.Unlock()

		if !ok {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"errors":["permission denied"]}`))

			return
		}

		if !strings.HasSuffix(r.URL.Path, "/v1/pki/certs/revoked") {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		_, _ = w.Write([]byte(`{"data":{"keys":["0a"]}}`))
	}))
	t.Cleanup(ts.Close)

	return ts
}

func TestTokenReloadRevokedCertificate(t *testing.T) {
	srv := &kvServer{}
	ts := revokedServer(t, srv)

	path := filepath.Join(t.TempDir(), "token")
	writeToken(t, path, "tok-1")
	srv.setToken("tok-1")

	p, err := NewProvider(context.Background(), ts.URL, "tok-1")
	if err != nil {
		t.Fatal(err)
	}

	if err = p.EnableTokenReload(path); err != nil {
		t.Fatal(err)
	}

	writeToken(t, path, "tok-22")
	srv.setToken("tok-22")

	state := `[{"ref":"pki-role:pki:web-role:web.example.com","serial":"0a"}]`

	before := srv.requests.Load()

	revoked, err := p.DeploymentHasRevokedCertificate(context.Background(), state)
	if err != nil {
		t.Fatalf("DeploymentHasRevokedCertificate() after a token change: %v", err)
	}

	if !revoked {
		t.Fatal("serial 0a is in the revoked list")
	}

	if n := srv.requests.Load() - before; n != 1 {
		t.Fatalf("%d requests, want 1: the change is seen by the stat, before any 403", n)
	}
}

func TestTokenReloadRevokedRetriesOn403(t *testing.T) {
	srv := &kvServer{}
	ts := revokedServer(t, srv)

	path := filepath.Join(t.TempDir(), "token")
	writeToken(t, path, "tok-1")
	srv.setToken("tok-1")

	p, err := NewProvider(context.Background(), ts.URL, "tok-1")
	if err != nil {
		t.Fatal(err)
	}

	if err = p.EnableTokenReload(path); err != nil {
		t.Fatal(err)
	}

	sameStampRewrite(t, path, "tok-2")
	srv.setToken("tok-2")

	before := srv.requests.Load()

	revoked, err := p.DeploymentHasRevokedCertificate(context.Background(), `[{"ref":"pki-role:pki:web-role:web.example.com","serial":"0a"}]`)
	if err != nil || !revoked {
		t.Fatalf("DeploymentHasRevokedCertificate() = %v, %v; want true, nil", revoked, err)
	}

	if n := srv.requests.Load() - before; n != 2 {
		t.Fatalf("%d requests, want exactly 2 (a 403, then one retry)", n)
	}
}

// sameStampRewrite rewrites path in place with a token of the same length and puts the mtime back:
// the stat sees no change.
func sameStampRewrite(t *testing.T, path, tok string) {
	t.Helper()

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if err = os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err = os.Chtimes(path, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
}

func TestTokenReloadPKIReadRetriesOn403(t *testing.T) {
	srv := &kvServer{}

	// LIST /v1/pki/certs/detailed with no certificate: after the retry the read fails on "not
	// found", not on the token, and the serial lookup is the only request of each attempt.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.requests.Add(1)

		srv.mu.Lock()
		ok := r.Header.Get("X-Vault-Token") == srv.token
		srv.mu.Unlock()

		if !ok {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"errors":["permission denied"]}`))

			return
		}

		_, _ = w.Write([]byte(`{"data":{"key_info":{}}}`))
	}))
	t.Cleanup(ts.Close)

	path := filepath.Join(t.TempDir(), "token")
	writeToken(t, path, "tok-1")
	srv.setToken("tok-1")

	p, err := NewProvider(context.Background(), ts.URL, "tok-1")
	if err != nil {
		t.Fatal(err)
	}

	if err = p.EnableTokenReload(path); err != nil {
		t.Fatal(err)
	}

	sameStampRewrite(t, path, "tok-2")
	srv.setToken("tok-2")

	before := srv.requests.Load()

	if _, err = p.GetSecret(context.Background(), "pki:pki:web.example.com"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("GetSecret() error = %v, want not found after the retry", err)
	}

	if n := srv.requests.Load() - before; n != 2 {
		t.Fatalf("%d requests, want exactly 2 (a 403, then one retry)", n)
	}

	sameStampRewrite(t, path, "tok-3")
	srv.setToken("tok-3")

	before = srv.requests.Load()

	if _, err = p.ResolveSecretReferences(context.Background(), map[string]string{"CERT": "pki:pki:web.example.com"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("ResolveSecretReferences() error = %v, want not found after the retry", err)
	}

	if n := srv.requests.Load() - before; n != 2 {
		t.Fatalf("%d requests, want exactly 2 (a 403, then one retry)", n)
	}
}

func TestTokenReloadNoRetryWithoutReload(t *testing.T) {
	srv := &kvServer{}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	srv.setToken("other")

	p, err := NewProvider(context.Background(), ts.URL, "tok-1")
	if err != nil {
		t.Fatal(err)
	}

	if _, err = p.GetSecret(context.Background(), "kv:secret:prod/web/db:PASS"); err == nil {
		t.Fatal("GetSecret() with a refused token must fail")
	}

	if n := srv.requests.Load(); n != 1 {
		t.Fatalf("%d requests, want 1: without reload a 403 is never retried", n)
	}
}

func TestTokenReloadNoRetryOnOtherErrors(t *testing.T) {
	p := &Provider{reload: &tokenReload{}}
	calls := 0
	want := errors.New("boom")

	if err := p.retryOnForbidden(context.Background(), func() error { calls++; return want }); !errors.Is(err, want) || calls != 1 {
		t.Fatalf("retryOnForbidden() = %v after %d calls, want boom after 1", err, calls)
	}
}

func TestEnableTokenReloadMissingFile(t *testing.T) {
	p, err := NewProvider(context.Background(), "http://127.0.0.1:1", "tok")
	if err != nil {
		t.Fatal(err)
	}

	if err = p.EnableTokenReload(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, errTokenReload) {
		t.Fatalf("EnableTokenReload() error = %v, want errTokenReload", err)
	}
}

func TestTokenReloadUnreadableFileKeepsToken(t *testing.T) {
	srv := &kvServer{}
	p, path := reloadingProvider(t, srv, "tok-1")

	// stat fails: the file is gone.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	if _, err := p.GetSecret(context.Background(), "kv:secret:prod/web/db:PASS"); err != nil {
		t.Fatalf("a missing file must keep the token in use: %v", err)
	}

	// read fails: the path is now a directory, whose stamp differs from the file's.
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := p.GetSecret(context.Background(), "kv:secret:prod/web/db:PASS"); err != nil {
		t.Fatalf("an unreadable file must keep the token in use: %v", err)
	}

	// forceRefresh after a 403 with the file gone, or unreadable: the token stays, the request fails.
	srv.setToken("tok-2")

	if _, err := p.GetSecret(context.Background(), "kv:secret:prod/web/db:PASS"); err == nil {
		t.Fatal("a refused token with an unreadable file must fail")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	if _, err := p.GetSecret(context.Background(), "kv:secret:prod/web/db:PASS"); err == nil {
		t.Fatal("a refused token with a missing file must fail")
	}
}

func TestForceRefreshWithoutReload(_ *testing.T) {
	p := &Provider{}
	p.forceRefresh() // no path: nothing to do, no panic
}

func TestGetConfigTokenReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	writeToken(t, path, "tok-1")

	t.Run("on with file", func(t *testing.T) {
		t.Setenv("SECRET_PROVIDER_SITE_URL", "http://bao:8200")
		t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN", "")
		t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN_FILE", path)
		t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN_FILE_RELOAD", "true")

		cfg, err := GetConfig()
		if err != nil {
			t.Fatal(err)
		}

		if !cfg.AccessTokenFileReload || cfg.AccessTokenFilePath != path || cfg.AccessToken != "tok-1" {
			t.Fatalf("GetConfig() = reload %v, path %q, token set %v", cfg.AccessTokenFileReload, cfg.AccessTokenFilePath, cfg.AccessToken == "tok-1")
		}
	})

	t.Run("on with env token", func(t *testing.T) {
		t.Setenv("SECRET_PROVIDER_SITE_URL", "http://bao:8200")
		t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN", "tok-env")
		t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN_FILE", "")
		t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN_FILE_RELOAD", "true")

		if _, err := GetConfig(); !errors.Is(err, errTokenReload) {
			t.Fatalf("GetConfig() error = %v, want errTokenReload", err)
		}
	})

	t.Run("off", func(t *testing.T) {
		t.Setenv("SECRET_PROVIDER_SITE_URL", "http://bao:8200")
		t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN", "tok-env")
		t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN_FILE", "")
		t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN_FILE_RELOAD", "")

		cfg, err := GetConfig()
		if err != nil {
			t.Fatal(err)
		}

		if cfg.AccessTokenFileReload || cfg.AccessToken != "tok-env" {
			t.Fatal("GetConfig() without the reload key must be upstream")
		}
	})
}
