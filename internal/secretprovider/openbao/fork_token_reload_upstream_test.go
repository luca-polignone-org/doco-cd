package openbao

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/kimdre/doco-cd/internal/encryption"
)

// Since v0.129.0 NewProviderWithConfig reads the token file before every operation and fails the
// operation on an unusable file. EnableTokenReload replaces that read: an emptied file keeps the
// token in use, as the fork's reload promises.
func TestTokenReloadReplacesUpstreamFileRead(t *testing.T) {
	srv := &kvServer{}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	path := filepath.Join(t.TempDir(), "token")
	writeToken(t, path, "tok-1")
	srv.setToken("tok-1")

	p, err := NewProviderWithConfig(context.Background(), &Config{SiteUrl: ts.URL, AccessTokenFile: path})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(p.Close)

	if err = p.EnableTokenReload(path); err != nil {
		t.Fatal(err)
	}

	if p.tokenFile != nil {
		t.Fatal("EnableTokenReload() must turn the upstream per-operation read off")
	}

	writeToken(t, path, "")

	if _, err = p.GetSecret(context.Background(), "kv:secret:prod/web/db:PASS"); err != nil {
		t.Fatalf("GetSecret() with an emptied file and the reload on: %v", err)
	}

	writeToken(t, path, "tok-2")
	srv.setToken("tok-2")

	if _, err = p.GetSecret(context.Background(), "kv:secret:prod/web/db:PASS"); err != nil {
		t.Fatalf("GetSecret() after the token changed: %v", err)
	}
}

// The reload parses the file as upstream does at startup, SOPS decryption included.
func TestTokenReloadDecryptsSOPS(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	encrypted, err := os.ReadFile(filepath.Join("..", "..", "encryption", "testdata", "encrypted.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	const decrypted = `this.is.encrypted: "yes"`

	srv := &kvServer{}
	p, _ := reloadingProvider(t, srv, "tok-1")

	// The SOPS format is detected from the extension, so the reloaded file is a .yaml one.
	path := filepath.Join(t.TempDir(), "token.yaml")
	writeToken(t, path, "tok-1")

	if err = p.EnableTokenReload(path); err != nil {
		t.Fatal(err)
	}

	if err = os.WriteFile(path, encrypted, 0o600); err != nil {
		t.Fatal(err)
	}

	srv.setToken(decrypted)

	if _, err = p.GetSecret(context.Background(), "kv:secret:prod/web/db:PASS"); err != nil {
		t.Fatalf("GetSecret() after a SOPS-encrypted token file: %v", err)
	}
}
