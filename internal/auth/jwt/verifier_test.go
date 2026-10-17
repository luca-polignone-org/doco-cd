package jwt

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	jwtlib "github.com/golang-jwt/jwt/v5"

	"github.com/kimdre/doco-cd/internal/rbac"
)

const (
	testIssuer   = "https://issuer.example"
	testAudience = "doco-cd"
)

var (
	testRSA   = mustRSA()
	testEC    = mustEC()
	testEC384 = mustEC384()
	baseTime  = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
)

func mustRSA() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}

	return k
}

func mustEC() *ecdsa.PrivateKey {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}

	return k
}

func mustEC384() *ecdsa.PrivateKey {
	k, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		panic(err)
	}

	return k
}

// jwksServer serves a mutable JWKS and counts requests.
type jwksServer struct {
	*httptest.Server

	mu      sync.Mutex
	body    []byte
	status  int
	hits    atomic.Int32
	entered chan struct{} // if set, signalled when a request arrives
	release chan struct{} // if set, requests wait for it
}

func newJWKSServer(t *testing.T, keys ...jose.JSONWebKey) *jwksServer {
	t.Helper()

	s := &jwksServer{status: http.StatusOK}
	s.setKeys(t, keys...)

	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s.hits.Add(1)

		s.mu.Lock()
		body, status, entered, release := s.body, s.status, s.entered, s.release
		s.mu.Unlock()

		if entered != nil {
			entered <- struct{}{}
		}

		if release != nil {
			<-release
		}

		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(s.Close)

	return s
}

func (s *jwksServer) setKeys(t *testing.T, keys ...jose.JSONWebKey) {
	t.Helper()

	b, err := json.Marshal(jose.JSONWebKeySet{Keys: keys})
	if err != nil {
		t.Fatal(err)
	}

	s.setBody(b)
}

func (s *jwksServer) setBody(b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.body = b
}

func (s *jwksServer) setStatus(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.status = code
}

func pub(key any, kid string) jose.JSONWebKey {
	return jose.JSONWebKey{Key: key, KeyID: kid, Use: "sig"}
}

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func newTestVerifier(t *testing.T, s *jwksServer, mod func(*Config)) (*Verifier, *clock) {
	t.Helper()

	clk := &clock{now: baseTime}
	cfg := Config{JWKSURL: s.URL, Issuer: testIssuer, Audience: testAudience, Now: clk.Now}

	if mod != nil {
		mod(&cfg)
	}

	v, err := NewVerifier(cfg)
	if err != nil {
		t.Fatal(err)
	}

	return v, clk
}

func claims(mod func(jwtlib.MapClaims)) jwtlib.MapClaims {
	c := jwtlib.MapClaims{
		"sub": "alice",
		"iss": testIssuer,
		"aud": testAudience,
		"exp": jwtlib.NewNumericDate(baseTime.Add(time.Hour)),
	}

	if mod != nil {
		mod(c)
	}

	return c
}

func sign(t *testing.T, method jwtlib.SigningMethod, key any, kid string, c jwtlib.MapClaims) string {
	t.Helper()

	tok := jwtlib.NewWithClaims(method, c)
	if kid != "" {
		tok.Header["kid"] = kid
	}

	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}

	return s
}

func rs256(t *testing.T, kid string, c jwtlib.MapClaims) string {
	t.Helper()

	return sign(t, jwtlib.SigningMethodRS256, testRSA, kid, c)
}

func wantInvalid(t *testing.T, v *Verifier, raw string) {
	t.Helper()

	_, err := v.Verify(context.Background(), raw)
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("want ErrInvalidToken, got %v", err)
	}
}

func wantPrincipal(t *testing.T, v *Verifier, raw string, want rbac.Principal) {
	t.Helper()

	got, err := v.Verify(context.Background(), raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got.Subject != want.Subject || strings.Join(got.Groups, ",") != strings.Join(want.Groups, ",") || (got.Groups == nil) != (want.Groups == nil) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestVerifyValid(t *testing.T) {
	s := newJWKSServer(t, pub(&testRSA.PublicKey, "rsa"), pub(&testEC.PublicKey, "ec"))
	v, _ := newTestVerifier(t, s, nil)

	t.Run("RS256 with group list", func(t *testing.T) {
		tok := rs256(t, "rsa", claims(func(c jwtlib.MapClaims) { c["groups"] = []string{"a", "b"} }))
		wantPrincipal(t, v, tok, rbac.Principal{Subject: "alice", Groups: []string{"a", "b"}})
	})

	t.Run("ES256 with group string", func(t *testing.T) {
		tok := sign(t, jwtlib.SigningMethodES256, testEC, "ec", claims(func(c jwtlib.MapClaims) { c["groups"] = "a" }))
		wantPrincipal(t, v, tok, rbac.Principal{Subject: "alice", Groups: []string{"a"}})
	})

	t.Run("missing groups", func(t *testing.T) {
		wantPrincipal(t, v, rs256(t, "rsa", claims(nil)), rbac.Principal{Subject: "alice"})
	})

	t.Run("audience list containing ours", func(t *testing.T) {
		tok := rs256(t, "rsa", claims(func(c jwtlib.MapClaims) { c["aud"] = []string{"other", testAudience} }))
		wantPrincipal(t, v, tok, rbac.Principal{Subject: "alice"})
	})

	t.Run("expired 30s ago is within leeway", func(t *testing.T) {
		tok := rs256(t, "rsa", claims(func(c jwtlib.MapClaims) { c["exp"] = jwtlib.NewNumericDate(baseTime.Add(-30 * time.Second)) }))
		wantPrincipal(t, v, tok, rbac.Principal{Subject: "alice"})
	})
}

func TestVerifyCustomGroupsClaim(t *testing.T) {
	s := newJWKSServer(t, pub(&testRSA.PublicKey, "rsa"))
	v, _ := newTestVerifier(t, s, func(c *Config) { c.GroupsClaim = "roles" })

	tok := rs256(t, "rsa", claims(func(c jwtlib.MapClaims) {
		c["roles"] = []string{"ops"}
		c["groups"] = []string{"ignored"}
	}))
	wantPrincipal(t, v, tok, rbac.Principal{Subject: "alice", Groups: []string{"ops"}})
}

func TestVerifyRejects(t *testing.T) {
	s := newJWKSServer(t, pub(&testRSA.PublicKey, "rsa"), pub(&testEC384.PublicKey, "ec384"))
	v, _ := newTestVerifier(t, s, nil)

	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: mustMarshalPKIX(t, &testRSA.PublicKey)})
	noneTok := sign(t, jwtlib.SigningMethodNone, jwtlib.UnsafeAllowNoneSignatureType, "rsa", claims(nil))
	hsTok := sign(t, jwtlib.SigningMethodHS256, pemBytes, "rsa", claims(nil))
	missingExp := claims(func(c jwtlib.MapClaims) { delete(c, "exp") })
	missingSub := claims(func(c jwtlib.MapClaims) { delete(c, "sub") })

	cases := map[string]string{
		"alg none":             noneTok,
		"HS256 with PEM key":   hsTok,
		"wrong issuer":         rs256(t, "rsa", claims(func(c jwtlib.MapClaims) { c["iss"] = "https://evil.example" })),
		"wrong audience":       rs256(t, "rsa", claims(func(c jwtlib.MapClaims) { c["aud"] = "other" })),
		"expired 2 minutes":    rs256(t, "rsa", claims(func(c jwtlib.MapClaims) { c["exp"] = jwtlib.NewNumericDate(baseTime.Add(-2 * time.Minute)) })),
		"missing exp":          rs256(t, "rsa", missingExp),
		"nbf in 2 minutes":     rs256(t, "rsa", claims(func(c jwtlib.MapClaims) { c["nbf"] = jwtlib.NewNumericDate(baseTime.Add(2 * time.Minute)) })),
		"empty sub":            rs256(t, "rsa", claims(func(c jwtlib.MapClaims) { c["sub"] = "" })),
		"missing sub":          rs256(t, "rsa", missingSub),
		"non-string sub":       rs256(t, "rsa", claims(func(c jwtlib.MapClaims) { c["sub"] = 42 })),
		"sub apikey":           rs256(t, "rsa", claims(func(c jwtlib.MapClaims) { c["sub"] = rbac.APIKeySubject })),
		"sub role:admin":       rs256(t, "rsa", claims(func(c jwtlib.MapClaims) { c["sub"] = "role:admin" })),
		"group role:admin":     rs256(t, "rsa", claims(func(c jwtlib.MapClaims) { c["groups"] = []string{"dev", "role:admin"} })),
		"group apikey":         rs256(t, "rsa", claims(func(c jwtlib.MapClaims) { c["groups"] = "apikey" })),
		"groups with a number": rs256(t, "rsa", claims(func(c jwtlib.MapClaims) { c["groups"] = []int{1} })),
		"groups is a number":   rs256(t, "rsa", claims(func(c jwtlib.MapClaims) { c["groups"] = 7 })),
		"missing kid":          rs256(t, "", claims(nil)),
		"garbage":              "not-a-jwt",
		"wrong signature":      sign(t, jwtlib.SigningMethodRS256, mustRSA(), "rsa", claims(nil)),
		"ES256 with P-384 key": sign(t, jwtlib.SigningMethodES256, testEC, "ec384", claims(nil)),
	}

	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			wantInvalid(t, v, tok)
		})
	}
}

func mustMarshalPKIX(t *testing.T, pub any) []byte {
	t.Helper()

	b, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}

	return b
}

func TestUnknownKidRefreshIsRateLimited(t *testing.T) {
	s := newJWKSServer(t, pub(&testRSA.PublicKey, "rsa"))
	v, clk := newTestVerifier(t, s, nil)

	wantPrincipal(t, v, rs256(t, "rsa", claims(nil)), rbac.Principal{Subject: "alice"})

	if got := s.hits.Load(); got != 1 {
		t.Fatalf("initial load: %d requests, want 1", got)
	}

	unknown := rs256(t, "nope", claims(nil))
	wantInvalid(t, v, unknown)
	wantInvalid(t, v, unknown)

	if got := s.hits.Load(); got != 2 {
		t.Fatalf("after two unknown kids: %d requests, want 2", got)
	}

	clk.now = clk.now.Add(31 * time.Second)

	wantInvalid(t, v, unknown)

	if got := s.hits.Load(); got != 3 {
		t.Fatalf("after the interval: %d requests, want 3", got)
	}
}

func TestUnknownKidConcurrent(t *testing.T) {
	s := newJWKSServer(t, pub(&testRSA.PublicKey, "rsa"))
	v, _ := newTestVerifier(t, s, nil)

	wantPrincipal(t, v, rs256(t, "rsa", claims(nil)), rbac.Principal{Subject: "alice"})

	unknown := rs256(t, "nope", claims(nil))

	var wg sync.WaitGroup

	for range 50 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			_, err := v.Verify(context.Background(), unknown)
			if !errors.Is(err, ErrInvalidToken) {
				t.Errorf("want ErrInvalidToken, got %v", err)
			}
		}()
	}

	wg.Wait()

	if got := s.hits.Load(); got != 2 {
		t.Fatalf("%d requests, want 2 (initial load + one refresh)", got)
	}
}

func TestKnownKidDoesNotWaitForRefresh(t *testing.T) {
	s := newJWKSServer(t, pub(&testRSA.PublicKey, "rsa"))
	v, _ := newTestVerifier(t, s, nil)

	wantPrincipal(t, v, rs256(t, "rsa", claims(nil)), rbac.Principal{Subject: "alice"})

	s.mu.Lock()
	s.entered = make(chan struct{}, 1)
	s.release = make(chan struct{})
	entered, release := s.entered, s.release
	s.mu.Unlock()

	var once sync.Once

	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock() // never leave the handler blocked, even on failure

	done := make(chan error, 1)

	go func() {
		_, err := v.Verify(context.Background(), rs256(t, "nope", claims(nil)))
		done <- err
	}()

	select {
	case <-entered: // the refresh is now in flight and blocked in the handler
	case <-time.After(10 * time.Second):
		t.Fatal("refresh request never reached the server")
	}

	wantPrincipal(t, v, rs256(t, "rsa", claims(nil)), rbac.Principal{Subject: "alice"})

	unblock()

	select {
	case err := <-done:
		if !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("want ErrInvalidToken, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Verify did not return after the refresh was released")
	}
}

func TestCancelledContextStillFetchesOnce(t *testing.T) {
	s := newJWKSServer(t, pub(&testRSA.PublicKey, "rsa"))
	v, _ := newTestVerifier(t, s, nil)

	wantPrincipal(t, v, rs256(t, "rsa", claims(nil)), rbac.Principal{Subject: "alice"})

	newKey := mustRSA()
	s.setKeys(t, pub(&testRSA.PublicKey, "rsa"), pub(&newKey.PublicKey, "new"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := v.Verify(ctx, sign(t, jwtlib.SigningMethodRS256, newKey, "new", claims(nil)))
	if err != nil || got.Subject != "alice" {
		t.Fatalf("got %#v, %v", got, err)
	}

	wantInvalid(t, v, rs256(t, "nope", claims(nil)))

	if n := s.hits.Load(); n != 2 {
		t.Fatalf("%d requests, want 2 (initial load + one refresh)", n)
	}
}

func TestKeyRotation(t *testing.T) {
	s := newJWKSServer(t, pub(&testRSA.PublicKey, "old"))
	v, clk := newTestVerifier(t, s, nil)

	wantPrincipal(t, v, rs256(t, "old", claims(nil)), rbac.Principal{Subject: "alice"})
	wantInvalid(t, v, rs256(t, "nope", claims(nil))) // first reload: opens the rate-limit window

	newKey := mustRSA()
	s.setKeys(t, pub(&testRSA.PublicKey, "old"), pub(&newKey.PublicKey, "new"))

	tok := sign(t, jwtlib.SigningMethodRS256, newKey, "new", claims(nil))
	wantInvalid(t, v, tok) // inside the refresh interval

	clk.now = clk.now.Add(31 * time.Second)
	tok = sign(t, jwtlib.SigningMethodRS256, newKey, "new", claims(func(c jwtlib.MapClaims) {
		c["exp"] = jwtlib.NewNumericDate(clk.now.Add(time.Hour))
	}))
	wantPrincipal(t, v, tok, rbac.Principal{Subject: "alice"})
}

func TestUnreachableJWKS(t *testing.T) {
	s := newJWKSServer(t, pub(&testRSA.PublicKey, "rsa"))
	v, _ := newTestVerifier(t, s, nil)

	s.Close()
	wantInvalid(t, v, rs256(t, "rsa", claims(nil)))
}

func TestFailedRefreshKeepsKnownKeys(t *testing.T) {
	s := newJWKSServer(t, pub(&testRSA.PublicKey, "rsa"))
	v, clk := newTestVerifier(t, s, nil)

	wantPrincipal(t, v, rs256(t, "rsa", claims(nil)), rbac.Principal{Subject: "alice"})

	s.setStatus(http.StatusInternalServerError)

	clk.now = clk.now.Add(31 * time.Second)
	exp := claims(func(c jwtlib.MapClaims) { c["exp"] = jwtlib.NewNumericDate(clk.now.Add(time.Hour)) })
	wantInvalid(t, v, rs256(t, "nope", exp))
	wantPrincipal(t, v, rs256(t, "rsa", exp), rbac.Principal{Subject: "alice"})

	s.setBody([]byte("not json"))

	clk.now = clk.now.Add(31 * time.Second)
	exp = claims(func(c jwtlib.MapClaims) { c["exp"] = jwtlib.NewNumericDate(clk.now.Add(time.Hour)) })
	wantInvalid(t, v, rs256(t, "nope", exp))
	wantPrincipal(t, v, rs256(t, "rsa", exp), rbac.Principal{Subject: "alice"})
}

func TestInvalidJWKSEntriesAreIgnored(t *testing.T) {
	good, err := json.Marshal(pub(&testRSA.PublicKey, "good"))
	if err != nil {
		t.Fatal(err)
	}

	edPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	ed, _ := json.Marshal(pub(edPub, "ed"))
	private, _ := json.Marshal(jose.JSONWebKey{Key: testRSA, KeyID: "private"})
	noKid, _ := json.Marshal(jose.JSONWebKey{Key: &testRSA.PublicKey})
	oct, _ := json.Marshal(jose.JSONWebKey{Key: []byte("secret"), KeyID: "oct"})

	body := `{"keys":[{"kty":"zzz","kid":"unknown-kty"},"not-an-object",{"kty":"RSA","kid":"broken"},` +
		string(noKid) + `,` + string(ed) + `,` + string(private) + `,` + string(oct) + `,` + string(good) + `]}`

	s := newJWKSServer(t)
	s.setBody([]byte(body))
	v, _ := newTestVerifier(t, s, nil)

	wantPrincipal(t, v, rs256(t, "good", claims(nil)), rbac.Principal{Subject: "alice"})
	// A private key in the JWKS is never trusted, even for a token it signed.
	wantInvalid(t, v, rs256(t, "private", claims(nil)))
	wantInvalid(t, v, rs256(t, "ed", claims(nil)))
	wantInvalid(t, v, rs256(t, "oct", claims(nil)))
}

// The second lookup under refreshMu: a caller that lost the race for the lock
// finds the key loaded by the winner and must not fail on the rate limit.
func TestKeyLoadedWhileWaitingForRefreshLock(t *testing.T) {
	s := newJWKSServer(t)
	v, _ := newTestVerifier(t, s, nil)

	v.afterMiss = func() {
		v.mu.Lock()
		v.keys["late"] = &testRSA.PublicKey
		v.mu.Unlock()
	}

	if _, err := v.key(context.Background(), "late"); err != nil {
		t.Fatal(err)
	}

	if n := s.hits.Load(); n != 0 {
		t.Fatalf("%d requests, want 0", n)
	}
}

func TestNewVerifier(t *testing.T) {
	ok := Config{JWKSURL: "https://issuer.example/jwks", Issuer: testIssuer, Audience: testAudience}

	tests := []struct {
		name    string
		mod     func(*Config)
		wantErr bool
	}{
		{"https", nil, false},
		{"http 127.0.0.1", func(c *Config) { c.JWKSURL = "http://127.0.0.1:8080/jwks" }, false},
		{"http localhost", func(c *Config) { c.JWKSURL = "http://localhost/jwks" }, false},
		{"missing JWKS URL", func(c *Config) { c.JWKSURL = "" }, true},
		{"missing issuer", func(c *Config) { c.Issuer = "" }, true},
		{"missing audience", func(c *Config) { c.Audience = "" }, true},
		{"unparsable URL", func(c *Config) { c.JWKSURL = "http://[::1" }, true},
		{"http remote host", func(c *Config) { c.JWKSURL = "http://issuer.example/jwks" }, true},
		{"other scheme", func(c *Config) { c.JWKSURL = "ftp://localhost/jwks" }, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ok
			if tt.mod != nil {
				tt.mod(&cfg)
			}

			_, err := NewVerifier(cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestNewVerifierDefaults(t *testing.T) {
	v, err := NewVerifier(Config{JWKSURL: "https://issuer.example/jwks", Issuer: testIssuer, Audience: testAudience})
	if err != nil {
		t.Fatal(err)
	}

	if v.cfg.GroupsClaim != "groups" || v.cfg.Leeway != 60*time.Second || v.cfg.MinRefreshInterval != 30*time.Second ||
		v.cfg.HTTPClient == nil || v.cfg.HTTPClient.Timeout != 10*time.Second || v.cfg.Now == nil {
		t.Fatalf("unexpected defaults: %+v", v.cfg)
	}
}
