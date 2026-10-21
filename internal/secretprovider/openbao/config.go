package openbao

import (
	"fmt"

	"github.com/kimdre/doco-cd/internal/config"
)

type Config struct {
	SiteUrl         string `env:"SECRET_PROVIDER_SITE_URL,notEmpty"` // URL of the secret provider
	AccessToken     string `env:"SECRET_PROVIDER_ACCESS_TOKEN"`      // #nosec G117 -- Access token for authenticating with the secret provider
	AccessTokenFile string `env:"SECRET_PROVIDER_ACCESS_TOKEN_FILE"` // Path to a file containing the access token

	// Fork (spec 4, innesto 2): keep the token in use when the token file changes into something
	// unusable, and retry an idempotent read once on 403. See fork_token_reload.go.
	AccessTokenFileReload bool   `env:"SECRET_PROVIDER_ACCESS_TOKEN_FILE_RELOAD"`
	AccessTokenFilePath   string // the path of SECRET_PROVIDER_ACCESS_TOKEN_FILE, set by GetConfig
}

// GetConfig retrieves and parses the configuration for the Secrets Manager from environment variables.
func GetConfig() (*Config, error) {
	cfg := Config{}

	var mappings []config.EnvVarFileMapping

	err := config.ParseConfigFromEnv(&cfg, &mappings)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", config.ErrParseConfigFailed, err)
	}

	// Fork: read before the file block below, which puts the file content in AccessToken too.
	tokenFromEnv := cfg.AccessToken != ""

	if cfg.AccessTokenFile != "" {
		if cfg.AccessToken != "" {
			return nil, fmt.Errorf("%w: %w: SECRET_PROVIDER_ACCESS_TOKEN or SECRET_PROVIDER_ACCESS_TOKEN_FILE", config.ErrParseConfigFailed, config.ErrBothSecretsSet)
		}

		cfg.AccessToken, err = newAccessTokenFile(cfg.AccessTokenFile).Token()
		if err != nil {
			return nil, fmt.Errorf("%w: %w", config.ErrParseConfigFailed, err)
		}
	} else if cfg.AccessToken == "" {
		return nil, fmt.Errorf("%w: %w: SECRET_PROVIDER_ACCESS_TOKEN or SECRET_PROVIDER_ACCESS_TOKEN_FILE", config.ErrParseConfigFailed, config.ErrBothSecretsNotSet)
	}

	// Fork: the reload needs the path and refuses a token from the environment.
	cfg.AccessTokenFilePath = cfg.AccessTokenFile

	if err = cfg.ValidateTokenReload(tokenFromEnv); err != nil {
		return nil, fmt.Errorf("%w: %w", config.ErrParseConfigFailed, err)
	}

	return &cfg, nil
}
