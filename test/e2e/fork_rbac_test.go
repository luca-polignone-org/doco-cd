//go:build e2e

package e2e

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	jwtlib "github.com/golang-jwt/jwt/v5"
	"github.com/moby/moby/api/types/container"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	forkRBACStack    = "e2e-fork-rbac"
	forkRBACIssuer   = "https://issuer.e2e.invalid"
	forkRBACAudience = "doco-cd-e2e"
	forkRBACKeyID    = "e2e-key"
	// forkRBACJWKSURL is on the daemon's own loopback: the JWKS sidecar shares the daemon's
	// network namespace, and the verifier accepts plain http only for 127.0.0.1 and localhost.
	forkRBACJWKSURL = "http://127.0.0.1:8089/jwks.json"
	forkRBACAPIKey  = "e2e-fork-rbac-api-key" // #nosec G101 -- test fixture, not a real credential.
)

// forkRBACJWKSNginxConf serves /srv on the loopback port of forkRBACJWKSURL. It must not listen on
// port 80, which belongs to the daemon in the shared network namespace.
const forkRBACJWKSNginxConf = `user root;
pid /tmp/nginx.pid;
events {}
http {
    access_log off;
    server {
        listen 127.0.0.1:8089;
        location / {
            root /srv;
        }
    }
}
`

// startJWKSSidecar serves jwks in the daemon's network namespace, so the daemon reaches it on
// 127.0.0.1 without TLS, a host gateway or a published port. Call after Start: the verifier
// fetches the JWKS on the first token, not at startup.
func (h *Harness) startJWKSSidecar(jwks []byte) {
	h.t.Helper()

	jwksPath := filepath.Join(h.workDir, "jwks.json")
	confPath := filepath.Join(h.workDir, "jwks-nginx.conf")

	for path, content := range map[string][]byte{jwksPath: jwks, confPath: []byte(forkRBACJWKSNginxConf)} {
		// #nosec G306 -- public key material and a server config, read by the sidecar.
		if err := os.WriteFile(path, content, 0o644); err != nil {
			h.t.Fatalf("write %s: %v", path, err)
		}
	}

	daemonID := h.daemon.GetContainerID()

	sidecar, err := testcontainers.GenericContainer(h.ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: "nginx:1.29-alpine",
			Name:  h.containerName("jwks"),
			HostConfigModifier: func(hc *container.HostConfig) {
				hc.NetworkMode = container.NetworkMode("container:" + daemonID)
				hc.Binds = append(hc.Binds,
					confPath+":/etc/nginx/nginx.conf:ro",
					jwksPath+":/srv/jwks.json:ro",
				)
			},
			WaitingFor: wait.ForExec([]string{"wget", "-q", "-O", "/dev/null", forkRBACJWKSURL}).
				WithStartupTimeout(30 * time.Second).
				WithPollInterval(500 * time.Millisecond),
		},
		Started: true,
	})
	if err != nil {
		h.t.Fatalf("start JWKS sidecar: %v", err)
	}

	h.t.Cleanup(func() { h.terminateContainer(sidecar) })
}

// forkRBACKeys returns a fresh signing key and its public JWKS.
func forkRBACKeys(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}

	jwks, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
		{Key: &key.PublicKey, KeyID: forkRBACKeyID, Algorithm: "RS256", Use: "sig"},
	}})
	if err != nil {
		t.Fatalf("marshal JWKS: %v", err)
	}

	return key, jwks
}

// forkRBACToken mints a token for sub in the given groups.
func forkRBACToken(t *testing.T, key *rsa.PrivateKey, sub string, groups ...string) string {
	t.Helper()

	now := time.Now()
	token := jwtlib.NewWithClaims(jwtlib.SigningMethodRS256, jwtlib.MapClaims{
		"iss":    forkRBACIssuer,
		"aud":    forkRBACAudience,
		"sub":    sub,
		"groups": groups,
		"iat":    now.Unix(),
		"exp":    now.Add(time.Hour).Unix(),
	})
	token.Header["kid"] = forkRBACKeyID

	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	return signed
}

// daemonEndpoint returns the published HTTP endpoint of the daemon.
func (h *Harness) daemonEndpoint() string {
	h.t.Helper()

	endpoint, err := h.daemon.Endpoint(h.ctx, "http")
	if err != nil {
		h.t.Fatalf("get daemon API endpoint: %v", err)
	}

	return endpoint
}

// bearerAPIRequest sends an API request with a bearer token and returns the status and body.
func (h *Harness) bearerAPIRequest(method, path, token, body string) (int, string) {
	h.t.Helper()

	ctx, cancel := context.WithTimeout(h.ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, h.daemonEndpoint()+path, strings.NewReader(body))
	if err != nil {
		h.t.Fatalf("create API request %s %s: %v", method, path, err)
	}

	req.Header.Set("Authorization", "Bearer "+token)

	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("send API request %s %s: %v", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("read API response %s %s: %v", method, path, err)
	}

	return resp.StatusCode, string(data)
}

// bearerRoundTripper adds the bearer token to every MCP request.
type bearerRoundTripper struct {
	token string
	base  http.RoundTripper
}

func (rt bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	cloned.Header.Set("Authorization", "Bearer "+rt.token)

	return rt.base.RoundTrip(cloned)
}

// TestForkRBAC covers JWT authentication and RBAC on REST and MCP against a real daemon: a token
// of team-a, which may only get default/* projects, lists projects but cannot delete one through
// either interface, cannot run an unconfigured poll target, and with API_KEY_ENABLED=false the
// API key no longer authenticates.
func TestForkRBAC(t *testing.T) {
	t.Parallel()
	requireStandaloneSuite(t)

	h := NewHarness(t, "fork-rbac")
	if h.isSwarmMode() {
		t.Skip("a Swarm target is always denied by the compose policy")
	}

	files := map[string]string{
		"compose-policy.yaml": "version: 1\ntargets:\n  \"default/" + forkRBACStack + "\":\n" +
			"    repository: http://gitserver/fork-rbac.git\n    reference: main\n",
		"rbac.csv": "p, team-a, projects, get, default/*, allow\n",
	}

	for name, content := range files {
		path := filepath.Join(h.workDir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}

		h.BindDaemonFile(path, "/config/"+name)
	}

	key, jwks := forkRBACKeys(t)

	h.EnableAPI(forkRBACAPIKey)
	h.SetEnv("API_KEY_ENABLED", "false")
	h.SetEnv("MCP_ENABLED", "true")
	h.SetEnv("COMPOSE_POLICY_FILE", "/config/compose-policy.yaml")
	h.SetEnv("RBAC_POLICY_FILE", "/config/rbac.csv")
	h.SetEnv("JWT_JWKS_URL", forkRBACJWKSURL)
	h.SetEnv("JWT_ISSUER", forkRBACIssuer)
	h.SetEnv("JWT_AUDIENCE", forkRBACAudience)
	h.Start()
	h.startJWKSSidecar(jwks)

	// The project list must not be empty, or it answers 404 whatever the token may see.
	h.WaitForLog(deployCompletedLog, 120*time.Second)

	token := forkRBACToken(t, key, "alice", "team-a")

	// 1. REST: get is granted on default/*, delete is not.
	if status, body := h.bearerAPIRequest(http.MethodGet, "/v1/api/projects", token, ""); status != http.StatusOK || !strings.Contains(body, forkRBACStack) {
		t.Fatalf("GET projects: status = %d, body = %s; want 200 listing %s", status, body, forkRBACStack)
	}

	if status, body := h.bearerAPIRequest(http.MethodDelete, "/v1/api/project/x", token, ""); status != http.StatusForbidden {
		t.Fatalf("DELETE project: status = %d, body = %s; want 403", status, body)
	}

	// 2. MCP: destroy_project with the same token is forbidden, and the project survives.
	httpClient := &http.Client{Transport: bearerRoundTripper{token: token, base: http.DefaultTransport}}
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "doco-cd-e2e", Version: "test"}, nil)

	session, err := client.Connect(t.Context(), &sdkmcp.StreamableClientTransport{
		Endpoint:             h.daemonEndpoint() + "/mcp",
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connect MCP: %v", err)
	}

	t.Cleanup(func() { _ = session.Close() })

	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name:      "destroy_project",
		Arguments: map[string]any{"project_name": forkRBACStack},
	})
	if err != nil {
		t.Fatalf("call destroy_project: %v", err)
	}

	content, _ := json.Marshal(result.Content)
	if !result.IsError || !strings.Contains(string(content), "forbidden") {
		t.Fatalf("destroy_project = %s (error %v), want forbidden", content, result.IsError)
	}

	if h.ContainerID(forkRBACStack, "app") == "" {
		t.Fatal("app container must survive a forbidden destroy_project")
	}

	// 3. REST: an inline poll target that the operator did not configure is refused.
	pollBody := `[{"url": "http://gitserver/other.git", "reference": "main"}]`
	if status, body := h.bearerAPIRequest(http.MethodPost, "/v1/api/poll/run", token, pollBody); status != http.StatusBadRequest {
		t.Fatalf("POST poll/run: status = %d, body = %s; want 400", status, body)
	}

	// 4. With API_KEY_ENABLED=false the right x-api-key without a token is unauthenticated.
	h.APIRequest(http.MethodGet, "/v1/api/projects", http.StatusUnauthorized)
}
