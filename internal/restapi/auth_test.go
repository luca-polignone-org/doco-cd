package restapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/rbac"
)

type fakeVerifier struct{}

var errBadToken = errors.New("bad token")

func (fakeVerifier) Verify(_ context.Context, raw string) (rbac.Principal, error) {
	if raw == "good" {
		return rbac.Principal{Subject: "u", Groups: []string{"g"}}, nil
	}

	return rbac.Principal{}, errBadToken
}

func TestAuthenticate(t *testing.T) {
	const secret = "s3cret"

	apiKey := rbac.Principal{Subject: rbac.APIKeySubject}
	user := rbac.Principal{Subject: "u", Groups: []string{"g"}}

	tests := []struct {
		name    string
		auth    *Authenticator
		headers map[string]string
		want    rbac.Principal
		wantErr bool
	}{
		{"no JWT, right key", &Authenticator{APISecret: secret, APIKeyEnabled: true}, map[string]string{KeyHeader: secret}, apiKey, false},
		{"no JWT, wrong key", &Authenticator{APISecret: secret, APIKeyEnabled: true}, map[string]string{KeyHeader: "nope"}, rbac.Principal{}, true},
		{"no JWT, missing key", &Authenticator{APISecret: secret, APIKeyEnabled: true}, nil, rbac.Principal{}, true},
		{"no JWT, key disabled", &Authenticator{APISecret: secret}, map[string]string{KeyHeader: secret}, rbac.Principal{}, true},
		{"no JWT, empty secret", &Authenticator{APIKeyEnabled: true}, map[string]string{KeyHeader: ""}, rbac.Principal{}, true},
		{"JWT, good bearer", &Authenticator{JWT: fakeVerifier{}}, map[string]string{"Authorization": "Bearer good"}, user, false},
		{"JWT, lowercase scheme", &Authenticator{JWT: fakeVerifier{}}, map[string]string{"Authorization": "bearer good"}, user, false},
		{"JWT, bad bearer, no key fallback", &Authenticator{APISecret: secret, APIKeyEnabled: true, JWT: fakeVerifier{}}, map[string]string{"Authorization": "Bearer bad", KeyHeader: secret}, rbac.Principal{}, true},
		{"JWT, basic scheme", &Authenticator{JWT: fakeVerifier{}}, map[string]string{"Authorization": "Basic xxx"}, rbac.Principal{}, true},
		{"JWT, no separator", &Authenticator{JWT: fakeVerifier{}}, map[string]string{"Authorization": "Bearer"}, rbac.Principal{}, true},
		{"JWT, empty token", &Authenticator{JWT: fakeVerifier{}}, map[string]string{"Authorization": "Bearer  "}, rbac.Principal{}, true},
		{"JWT, key without Authorization", &Authenticator{APISecret: secret, APIKeyEnabled: true, JWT: fakeVerifier{}}, map[string]string{KeyHeader: secret}, apiKey, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			for k, v := range tt.headers {
				r.Header.Set(k, v)
			}

			got, err := tt.auth.Authenticate(r)
			if tt.wantErr {
				if !errors.Is(err, ErrUnauthenticated) {
					t.Fatalf("err = %v, want ErrUnauthenticated", err)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("principal = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestAuthenticateBadBearerWrapsVerifierError(t *testing.T) {
	a := &Authenticator{JWT: fakeVerifier{}}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer bad")

	_, err := a.Authenticate(r)
	if !errors.Is(err, ErrUnauthenticated) || !errors.Is(err, errBadToken) {
		t.Fatalf("err = %v, want both ErrUnauthenticated and the verifier error", err)
	}
}

func TestNewAuthenticator(t *testing.T) {
	cfg := &app.Config{ApiSecret: "k", APIKeyEnabled: true}
	v := fakeVerifier{}

	a := NewAuthenticator(cfg, v)
	if a.APISecret != "k" || !a.APIKeyEnabled || a.JWT != v {
		t.Fatalf("unexpected authenticator: %+v", a)
	}
}
