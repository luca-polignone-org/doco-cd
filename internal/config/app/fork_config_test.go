package app

import (
	"strings"
	"testing"
)

func setBaseEnv(t *testing.T) {
	t.Helper()

	t.Setenv("WEBHOOK_SECRET", "secret")
	t.Setenv("GIT_ACCESS_TOKEN", "token")
}

func TestGetConfigForkDefaults(t *testing.T) {
	setBaseEnv(t)

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("GetConfig() error = %v", err)
	}

	if !cfg.APIKeyEnabled {
		t.Error("APIKeyEnabled should default to true")
	}

	if cfg.JWTGroupsClaim != "groups" {
		t.Errorf("JWTGroupsClaim = %q, want groups", cfg.JWTGroupsClaim)
	}
}

func TestGetConfigForkAuthRules(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{
			name:    "JWT without issuer and audience",
			env:     map[string]string{"JWT_JWKS_URL": "https://idp/jwks", "RBAC_POLICY_FILE": "/p.csv", "COMPOSE_POLICY_FILE": "/c.yaml"},
			wantErr: "JWT_JWKS_URL requires JWT_ISSUER and JWT_AUDIENCE",
		},
		{
			name:    "JWT without RBAC",
			env:     map[string]string{"JWT_JWKS_URL": "https://idp/jwks", "JWT_ISSUER": "i", "JWT_AUDIENCE": "a"},
			wantErr: "JWT_JWKS_URL requires RBAC_POLICY_FILE",
		},
		{
			name:    "RBAC without compose policy",
			env:     map[string]string{"RBAC_POLICY_FILE": "/p.csv"},
			wantErr: "RBAC_POLICY_FILE requires COMPOSE_POLICY_FILE",
		},
		{
			name:    "RBAC with key disabled and no JWT",
			env:     map[string]string{"RBAC_POLICY_FILE": "/p.csv", "COMPOSE_POLICY_FILE": "/c.yaml", "API_KEY_ENABLED": "false"},
			wantErr: "RBAC_POLICY_FILE with API_KEY_ENABLED=false requires JWT_JWKS_URL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setBaseEnv(t)

			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			_, err := GetConfig()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("GetConfig() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestGetConfigForkAuthValid(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("JWT_JWKS_URL", "https://idp/jwks")
	t.Setenv("JWT_ISSUER", "i")
	t.Setenv("JWT_AUDIENCE", "a")
	t.Setenv("RBAC_POLICY_FILE", "/p.csv")
	t.Setenv("COMPOSE_POLICY_FILE", "/c.yaml")
	t.Setenv("API_KEY_ENABLED", "false")

	if _, err := GetConfig(); err != nil {
		t.Fatalf("GetConfig() error = %v", err)
	}
}

func TestGetConfigMCPWithJWTNeedsNoAPISecret(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("MCP_ENABLED", "true")
	t.Setenv("JWT_JWKS_URL", "https://idp/jwks")
	t.Setenv("JWT_ISSUER", "i")
	t.Setenv("JWT_AUDIENCE", "a")
	t.Setenv("RBAC_POLICY_FILE", "/p.csv")
	t.Setenv("COMPOSE_POLICY_FILE", "/c.yaml")

	if _, err := GetConfig(); err != nil {
		t.Fatalf("GetConfig() error = %v", err)
	}
}

func TestGetConfigMCPWithoutSecretOrJWTFails(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("MCP_ENABLED", "true")

	_, err := GetConfig()
	if err == nil || !strings.Contains(err.Error(), "MCP_ENABLED requires API_SECRET") {
		t.Fatalf("GetConfig() error = %v", err)
	}
}
