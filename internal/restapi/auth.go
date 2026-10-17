package restapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/rbac"
)

// TokenVerifier validates a raw bearer token and returns the principal it identifies.
type TokenVerifier interface {
	Verify(ctx context.Context, raw string) (rbac.Principal, error)
}

// Authenticator replaces ValidateApiKey: with JWT off and the key on it is exactly
// the upstream check; with JWT on, a present Bearer header decides alone, without
// falling back to the API key.
type Authenticator struct {
	APISecret     string
	APIKeyEnabled bool
	JWT           TokenVerifier // nil = JWT disabled
}

// ErrUnauthenticated is returned when the request carries no valid credentials.
var ErrUnauthenticated = errors.New("unauthenticated")

// NewAuthenticator builds an Authenticator from the app config and an optional JWT verifier.
func NewAuthenticator(cfg *app.Config, jwt TokenVerifier) *Authenticator {
	return &Authenticator{APISecret: cfg.ApiSecret, APIKeyEnabled: cfg.APIKeyEnabled, JWT: jwt}
}

// Authenticate returns the principal of the request or ErrUnauthenticated.
func (a *Authenticator) Authenticate(r *http.Request) (rbac.Principal, error) {
	if a.JWT != nil {
		if header := r.Header.Get("Authorization"); header != "" {
			scheme, token, ok := strings.Cut(header, " ")
			if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
				return rbac.Principal{}, ErrUnauthenticated
			}

			p, err := a.JWT.Verify(r.Context(), strings.TrimSpace(token))
			if err != nil {
				return rbac.Principal{}, fmt.Errorf("%w: %w", ErrUnauthenticated, err)
			}

			return p, nil
		}
	}

	if a.APIKeyEnabled && ValidateApiKey(r, a.APISecret) {
		return rbac.Principal{Subject: rbac.APIKeySubject}, nil
	}

	return rbac.Principal{}, ErrUnauthenticated
}
