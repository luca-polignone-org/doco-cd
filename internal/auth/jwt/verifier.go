// Package jwt verifies OIDC bearer tokens against the issuer's JWKS and turns
// them into an rbac.Principal.
package jwt

import (
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	jwtlib "github.com/golang-jwt/jwt/v5"

	"github.com/kimdre/doco-cd/internal/rbac"
)

// ErrInvalidToken is returned (wrapped) for every token that must not be trusted.
var ErrInvalidToken = errors.New("invalid token")

const (
	maxJWKSBytes = 1 << 20
	fetchTimeout = 10 * time.Second
	rolePrefix   = "role:"
)

// Config configures a Verifier.
type Config struct {
	JWKSURL, Issuer, Audience, GroupsClaim string
	HTTPClient                             *http.Client     // nil: client with a 10s timeout
	Leeway                                 time.Duration    // 0: 60s
	MinRefreshInterval                     time.Duration    // 0: 30s
	Now                                    func() time.Time // nil: time.Now
}

// Verifier validates the JWTs of the configured issuer against its JWKS. Only
// RS256 and ES256 are accepted: no none, no HS*, so a public key can never be
// used as an HMAC secret.
type Verifier struct {
	cfg     Config
	jwksURL *url.URL
	parser  *jwtlib.Parser

	mu          sync.RWMutex // protects keys and lastRefresh
	keys        map[string]any
	lastRefresh time.Time

	refreshMu sync.Mutex // one fetch at a time; lookups of known kids never wait for it
}

// NewVerifier validates the configuration. It does not contact the issuer.
func NewVerifier(cfg Config) (*Verifier, error) {
	if cfg.JWKSURL == "" || cfg.Issuer == "" || cfg.Audience == "" {
		return nil, errors.New("JWT_JWKS_URL, JWT_ISSUER and JWT_AUDIENCE are required")
	}

	u, err := url.Parse(cfg.JWKSURL)
	if err != nil {
		return nil, fmt.Errorf("invalid JWT_JWKS_URL: %w", err)
	}

	local := u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost"
	if u.Scheme != "https" && (u.Scheme != "http" || !local) {
		return nil, errors.New("JWT_JWKS_URL must use https")
	}

	if cfg.GroupsClaim == "" {
		cfg.GroupsClaim = "groups"
	}

	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: fetchTimeout}
	}

	if cfg.Leeway == 0 {
		cfg.Leeway = 60 * time.Second
	}

	if cfg.MinRefreshInterval == 0 {
		cfg.MinRefreshInterval = 30 * time.Second
	}

	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	return &Verifier{
		cfg:     cfg,
		jwksURL: u,
		keys:    map[string]any{},
		parser: jwtlib.NewParser(
			jwtlib.WithValidMethods([]string{"RS256", "ES256"}),
			jwtlib.WithIssuer(cfg.Issuer),
			jwtlib.WithAudience(cfg.Audience),
			jwtlib.WithLeeway(cfg.Leeway),
			jwtlib.WithExpirationRequired(),
			jwtlib.WithTimeFunc(cfg.Now),
		),
	}, nil
}

// Verify checks the signature and the registered claims of raw and returns the
// principal it identifies. Every failure wraps ErrInvalidToken.
func (v *Verifier) Verify(ctx context.Context, raw string) (rbac.Principal, error) {
	claims := jwtlib.MapClaims{}

	_, err := v.parser.ParseWithClaims(raw, claims, func(t *jwtlib.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("missing kid")
		}

		return v.key(ctx, kid)
	})
	if err != nil {
		return rbac.Principal{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}

	sub, _ := claims["sub"].(string)
	if sub == "" {
		return rbac.Principal{}, fmt.Errorf("%w: missing sub", ErrInvalidToken)
	}

	groups, err := groupsOf(claims[v.cfg.GroupsClaim])
	if err != nil {
		return rbac.Principal{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}

	// A token must not take the identity of the API key or of a policy role: in
	// the policy CSV they are subjects like any other.
	if sub == rbac.APIKeySubject || strings.HasPrefix(sub, rolePrefix) {
		return rbac.Principal{}, fmt.Errorf("%w: reserved subject %q", ErrInvalidToken, sub)
	}

	for _, g := range groups {
		if strings.HasPrefix(g, rolePrefix) || g == rbac.APIKeySubject {
			return rbac.Principal{}, fmt.Errorf("%w: reserved group %q", ErrInvalidToken, g)
		}
	}

	return rbac.Principal{Subject: sub, Groups: groups}, nil
}

func groupsOf(raw any) ([]string, error) {
	switch g := raw.(type) {
	case nil:
		return nil, nil
	case string:
		return []string{g}, nil
	case []any:
		out := make([]string, 0, len(g))

		for _, item := range g {
			s, ok := item.(string)
			if !ok {
				return nil, errors.New("groups claim must contain strings")
			}

			out = append(out, s)
		}

		return out, nil
	default:
		return nil, errors.New("groups claim must be a string or a list of strings")
	}
}

// key returns the key of kid. On an unknown kid it reloads the JWKS at most once
// per MinRefreshInterval: one fetch at a time (refreshMu), while known kids are
// read under RLock without waiting for it. The fetch uses a context detached
// from the request, with its own timeout, so a cancelled request does not burn
// the refresh slot for nothing.
func (v *Verifier) key(ctx context.Context, kid string) (any, error) {
	if k, ok := v.lookup(kid); ok {
		return k, nil
	}

	v.refreshMu.Lock()
	defer v.refreshMu.Unlock()

	// Another goroutine may have reloaded while this one was waiting.
	if k, ok := v.lookup(kid); ok {
		return k, nil
	}

	now := v.cfg.Now()

	v.mu.RLock()
	recent := !v.lastRefresh.IsZero() && now.Sub(v.lastRefresh) < v.cfg.MinRefreshInterval
	v.mu.RUnlock()

	if recent {
		return nil, fmt.Errorf("unknown kid %q", kid)
	}

	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
	defer cancel()

	keys, err := v.fetch(fetchCtx)

	v.mu.Lock()
	v.lastRefresh = now

	if err == nil {
		v.keys = keys // a failed fetch leaves the known keys as they are
	}
	v.mu.Unlock()

	if err != nil {
		return nil, err
	}

	if k, ok := v.lookup(kid); ok {
		return k, nil
	}

	return nil, fmt.Errorf("unknown kid %q", kid)
}

func (v *Verifier) lookup(kid string) (any, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	k, ok := v.keys[kid]

	return k, ok
}

func (v *Verifier) fetch(ctx context.Context) (map[string]any, error) {
	// Built by hand: the URL was parsed in NewVerifier, so there is no error path.
	req := (&http.Request{Method: http.MethodGet, URL: v.jwksURL, Header: http.Header{}}).WithContext(ctx)

	resp, err := v.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch jwks: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch jwks: status %d", resp.StatusCode)
	}

	var raw struct {
		Keys []json.RawMessage `json:"keys"`
	}

	if err = json.NewDecoder(io.LimitReader(resp.Body, maxJWKSBytes)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode jwks: %w", err)
	}

	keys := map[string]any{}

	for _, item := range raw.Keys {
		var jwk jose.JSONWebKey
		if jwk.UnmarshalJSON(item) != nil || jwk.KeyID == "" || !jwk.Valid() || !jwk.IsPublic() {
			continue // an unreadable key does not invalidate the others
		}

		switch k := jwk.Key.(type) {
		case *rsa.PublicKey, *ecdsa.PublicKey:
			keys[jwk.KeyID] = k
		}
	}

	return keys, nil
}
