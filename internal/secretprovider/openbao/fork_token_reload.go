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
// beyond token_max_ttl or restarts. Since v0.129.0 upstream reads SECRET_PROVIDER_ACCESS_TOKEN_FILE
// before every operation and fails the operation when the file is missing, empty or not
// decodable. With SECRET_PROVIDER_ACCESS_TOKEN_FILE_RELOAD the fork keeps its own semantics
// instead: every public method stats the file first and swaps the token of both clients when
// mtime or size changed, an unusable file keeps the token in use, and a 403 on an idempotent read
// re-reads the file once and retries once; issuance (pki-role) is never retried.

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

	// Defensive: GetConfig never gets here with both variables set, because it returns
	// ErrBothSecretsSet first. The check keeps the rule true for any other caller.
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

	// failure is the last failure logged ("" when the last read went well): the same failure is
	// logged once, not on every call, and the recovery is logged once.
	failure string
	// failedOnStat is true when that failure came from os.Stat: only then does an unchanged
	// stamp mean the file is back. A read failure keeps the stamp, so the stamp proves nothing.
	failedOnStat bool
}

// failLocked logs a reload failure unless it is the one already logged.
func (r *tokenReload) failLocked(msg string, err error, onStat bool) {
	r.failedOnStat = onStat

	key := msg
	if err != nil {
		key += ": " + err.Error()
	}

	if key == r.failure {
		return
	}

	r.failure = key

	attrs := []any{slog.String("path", r.path)}
	if err != nil {
		attrs = append(attrs, slog.Any("error", err))
	}

	slog.Error(msg, attrs...)
}

// recoveredLocked logs once that the file is usable again after a failure.
func (r *tokenReload) recoveredLocked() {
	if r.failure == "" {
		return
	}

	prev := r.failure
	r.failure, r.failedOnStat = "", false

	slog.Warn("openbao token file usable again", slog.String("path", r.path), slog.String("previous", prev))
}

// EnableTokenReload turns on the reload of the token from path.
func (p *Provider) EnableTokenReload(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return errors.Join(errTokenReload, err)
	}

	p.reload = &tokenReload{path: path, size: st.Size(), mtime: st.ModTime()}
	// The reload replaces the upstream per-operation read (namespaceClient): that read fails the
	// operation on an unusable file, where the reload keeps the token in use.
	p.tokenFile = nil

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
		r.failLocked("openbao token file unreadable, keeping the token in use", err, true)
		return
	}

	if st.Size() == r.size && st.ModTime().Equal(r.mtime) {
		// The file in use is back after it was missing for a while; after a read failure the
		// same stamp says nothing about the content, so stay quiet until a read succeeds.
		if r.failedOnStat {
			r.recoveredLocked()
		}

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
		r.failLocked("openbao token file unreadable, keeping the token in use", err, false)
		return
	}

	// parseAccessToken is the upstream parser (trim, SOPS decryption), so a reloaded token is read
	// exactly as the startup one.
	tok, err := parseAccessToken(data, r.path)
	if errors.Is(err, errEmptyAccessTokenFile) {
		r.failLocked("openbao token file empty, keeping the token in use", nil, false)
		return
	}

	if err != nil {
		r.failLocked("openbao token file not decodable, keeping the token in use", err, false)
		return
	}

	r.size, r.mtime = st.Size(), st.ModTime()
	r.recoveredLocked()

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
