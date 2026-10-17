package secretprovider_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kimdre/doco-cd/internal/secretprovider"
	"github.com/kimdre/doco-cd/internal/secretprovider/openbao"
)

// TestInitializeOpenBaoTokenReload covers the fork hook in Initialize: with
// SECRET_PROVIDER_ACCESS_TOKEN_FILE_RELOAD the provider starts watching the token file.
func TestInitializeOpenBaoTokenReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("tok-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("SECRET_PROVIDER_SITE_URL", "http://127.0.0.1:1")
	t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN", "")
	t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN_FILE", path)
	t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN_FILE_RELOAD", "true")

	p, err := secretprovider.Initialize(t.Context(), openbao.Name, "test")
	if err != nil {
		t.Fatalf("Initialize() with reload: %v", err)
	}

	p.Close()

	// A missing token file fails GetConfig already, before the hook; the hook's own stat error is
	// covered by TestEnableTokenReloadMissingFile in the openbao package.
	t.Setenv("SECRET_PROVIDER_ACCESS_TOKEN_FILE", filepath.Join(t.TempDir(), "missing"))

	if _, err = secretprovider.Initialize(t.Context(), openbao.Name, "test"); err == nil {
		t.Fatal("Initialize() with a missing token file must fail")
	}
}
