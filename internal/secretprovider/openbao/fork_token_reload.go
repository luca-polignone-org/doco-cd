package openbao

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	openbao "github.com/openbao/openbao/api/v2"
)

// Token reload (Portico fork, spec 4, innesto 2). A bao agent rewrites its sink when it renews
// beyond token_max_ttl or restarts; upstream reads SECRET_PROVIDER_ACCESS_TOKEN_FILE once. With
// SECRET_PROVIDER_ACCESS_TOKEN_FILE_RELOAD every public method stats the file first and swaps the
// token of both clients when mtime or size changed. A 403 on an idempotent read re-reads the file
// once and retries once; issuance (pki-role) is never retried.

var errTokenReload = errors.New("token reload")

// ValidateTokenReload refuses the reload without a token file, or with a token in the
// environment: that token could never be reloaded.
func (c *Config) ValidateTokenReload(tokenFromEnv bool) error {
	if !c.AccessTokenFileReload {
		return nil
	}

	if strings.TrimSpace(c.AccessTokenFilePath) == "" {
		return errors.Join(errTokenReload, errors.New("SECRET_PROVIDER_ACCESS_TOKEN_FILE_RELOAD needs SECRET_PROVIDER_ACCESS_TOKEN_FILE"))
	}

	if tokenFromEnv {
		return errors.Join(errTokenReload, errors.New("SECRET_PROVIDER_ACCESS_TOKEN_FILE_RELOAD cannot be used with SECRET_PROVIDER_ACCESS_TOKEN"))
	}

	return nil
}

type tokenReload struct {
	mu    sync.Mutex
	path  string
	size  int64
	mtime time.Time
}

// EnableTokenReload turns on the reload of the token from path.
func (p *Provider) EnableTokenReload(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return errors.Join(errTokenReload, err)
	}

	p.reload = &tokenReload{path: path, size: st.Size(), mtime: st.ModTime()}

	return nil
}

// refreshToken swaps the token when the file changed. An unreadable or empty file keeps the token
// in use; the log never carries the content.
func (p *Provider) refreshToken() {
	r := p.reload
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	st, err := os.Stat(r.path)
	if err != nil {
		slog.Error("openbao token file unreadable, keeping the token in use", slog.String("path", r.path), slog.Any("error", err))
		return
	}

	if st.Size() == r.size && st.ModTime().Equal(r.mtime) {
		return
	}

	p.loadTokenLocked(r, st)
}

// forceRefresh re-reads the file even if the stamp did not change (after a 403).
func (p *Provider) forceRefresh() {
	r := p.reload
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if st, err := os.Stat(r.path); err == nil {
		p.loadTokenLocked(r, st)
	}
}

func (p *Provider) loadTokenLocked(r *tokenReload, st os.FileInfo) {
	data, err := os.ReadFile(r.path) // #nosec G304 -- operator-configured token file
	if err != nil {
		slog.Error("openbao token file unreadable, keeping the token in use", slog.String("path", r.path), slog.Any("error", err))
		return
	}

	tok := strings.TrimSpace(string(data))
	if tok == "" {
		slog.Error("openbao token file empty, keeping the token in use", slog.String("path", r.path))
		return
	}

	r.size, r.mtime = st.Size(), st.ModTime()

	// WithNamespace copies the client struct (c2 := *c) without its lock, so a SetToken running at
	// the same time is a data race: tokenMu orders the swap against every nsClient/nsIssuer.
	p.tokenMu.Lock()
	p.Client.SetToken(tok)
	p.issuer.SetToken(tok)
	p.tokenMu.Unlock()
}

// nsClient is p.Client.WithNamespace under the token read lock; every read of the provider goes
// through it, so a request never starts from a client copied halfway through a token swap.
func (p *Provider) nsClient(namespace string) *openbao.Client {
	p.tokenMu.RLock()
	defer p.tokenMu.RUnlock()

	return p.Client.WithNamespace(namespace)
}

// nsIssuer is the same for the issuance client.
func (p *Provider) nsIssuer(namespace string) *openbao.Client {
	p.tokenMu.RLock()
	defer p.tokenMu.RUnlock()

	return p.issuer.WithNamespace(namespace)
}

func isForbidden(err error) bool {
	var re *openbao.ResponseError

	return errors.As(err, &re) && re.StatusCode == http.StatusForbidden
}

// retryOnForbidden runs an idempotent read; on 403 with reload enabled it re-reads the token and
// runs it once more.
func (p *Provider) retryOnForbidden(_ context.Context, fn func() error) error {
	err := fn()
	if err == nil || p.reload == nil || !isForbidden(err) {
		return err
	}

	p.forceRefresh()

	return fn()
}
