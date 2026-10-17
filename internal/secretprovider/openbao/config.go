package openbao

import (
	"fmt"
	"os"

	"github.com/kimdre/doco-cd/internal/config"
)

type Config struct {
	SiteUrl         string `env:"SECRET_PROVIDER_SITE_URL,notEmpty"`      // URL of the secret provider
	AccessToken     string `env:"SECRET_PROVIDER_ACCESS_TOKEN"`           // #nosec G117 -- Access token for authenticating with the secret provider
	AccessTokenFile string `env:"SECRET_PROVIDER_ACCESS_TOKEN_FILE,file"` // Path to a file containing the access token

	// Fork (spec 4, innesto 2): re-read the token file when it changes, for a bao agent sink.
	AccessTokenFileReload bool   `env:"SECRET_PROVIDER_ACCESS_TOKEN_FILE_RELOAD"`
	AccessTokenFilePath   string // the raw path of SECRET_PROVIDER_ACCESS_TOKEN_FILE, set by GetConfig
}

// GetConfig retrieves and parses the configuration for the Secrets Manager from environment variables.
func GetConfig() (*Config, error) {
	cfg := Config{}

	mappings := []config.EnvVarFileMapping{
		{EnvName: "SECRET_PROVIDER_ACCESS_TOKEN", EnvValue: &cfg.AccessToken, FileValue: &cfg.AccessTokenFile, AllowUnset: false},
	}

	// Read before ParseConfigFromEnv: afterwards the file content is in AccessToken too, and the
	// two sources can no longer be told apart.
	tokenFromEnv := os.Getenv("SECRET_PROVIDER_ACCESS_TOKEN") != ""

	err := config.ParseConfigFromEnv(&cfg, &mappings)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", config.ErrParseConfigFailed, err)
	}

	cfg.AccessTokenFilePath = os.Getenv("SECRET_PROVIDER_ACCESS_TOKEN_FILE")

	if err = cfg.ValidateTokenReload(tokenFromEnv); err != nil {
		return nil, fmt.Errorf("%w: %w", config.ErrParseConfigFailed, err)
	}

	return &cfg, nil
}
