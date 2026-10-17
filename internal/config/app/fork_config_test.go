package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setBaseEnv(t *testing.T) {
	t.Helper()

	t.Setenv("WEBHOOK_SECRET", "secret")
	t.Setenv("GIT_ACCESS_TOKEN", "token")

	// Do not inherit these from the ambient environment (CI exports API_SECRET).
	for _, k := range []string{
		"API_SECRET", "API_SECRET_FILE", "MCP_ENABLED",
		"JWT_JWKS_URL", "JWT_ISSUER", "JWT_AUDIENCE", "JWT_GROUPS_CLAIM",
		"RBAC_POLICY_FILE", "COMPOSE_POLICY_FILE",
		"HOOK_PRE_ROLLBACK_URL", "HOOK_OUTCOME_URL", "HOOK_SECRET", "HOOK_SECRET_FILE",
	} {
		t.Setenv(k, "")
	}

	// notEmpty keys with a default: register the restore, then really unset.
	for _, k := range []string{"API_KEY_ENABLED"} {
		t.Setenv(k, "")

		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
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

func TestGetConfigForkHookRules(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{
			name:    "pre_rollback URL without secret",
			env:     map[string]string{"HOOK_PRE_ROLLBACK_URL": "https://gate/pre"},
			wantErr: "HOOK_PRE_ROLLBACK_URL and HOOK_OUTCOME_URL require HOOK_SECRET or HOOK_SECRET_FILE",
		},
		{
			name:    "outcome URL without secret",
			env:     map[string]string{"HOOK_OUTCOME_URL": "https://gate/out"},
			wantErr: "HOOK_PRE_ROLLBACK_URL and HOOK_OUTCOME_URL require HOOK_SECRET or HOOK_SECRET_FILE",
		},
		{
			name:    "pre_rollback URL with a non-http scheme",
			env:     map[string]string{"HOOK_PRE_ROLLBACK_URL": "ftp://gate/pre", "HOOK_SECRET": "s"},
			wantErr: "HOOK_PRE_ROLLBACK_URL must be an http or https URL",
		},
		{
			name:    "outcome URL without host",
			env:     map[string]string{"HOOK_OUTCOME_URL": "https:///out", "HOOK_SECRET": "s"},
			wantErr: "HOOK_OUTCOME_URL must be an http or https URL",
		},
		{
			name:    "outcome URL unparsable",
			env:     map[string]string{"HOOK_OUTCOME_URL": "http://[::1", "HOOK_SECRET": "s"},
			wantErr: "HOOK_OUTCOME_URL must be an http or https URL",
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

func TestGetConfigForkHooksValid(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("HOOK_PRE_ROLLBACK_URL", "https://gate/pre")
	t.Setenv("HOOK_OUTCOME_URL", "http://gate/out")
	t.Setenv("HOOK_SECRET", "s")

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("GetConfig() error = %v", err)
	}

	if cfg.HookSecret != "s" {
		t.Error("HookSecret not loaded")
	}
}

func TestGetConfigForkHookSecretFromFile(t *testing.T) {
	setBaseEnv(t)

	f := filepath.Join(t.TempDir(), "hook-secret")
	if err := os.WriteFile(f, []byte("from-file"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOOK_PRE_ROLLBACK_URL", "https://gate/pre")
	t.Setenv("HOOK_SECRET_FILE", f)

	cfg, err := GetConfig()
	if err != nil {
		t.Fatalf("GetConfig() error = %v", err)
	}

	if cfg.HookSecret != "from-file" {
		t.Error("HookSecret not loaded from HOOK_SECRET_FILE")
	}
}
