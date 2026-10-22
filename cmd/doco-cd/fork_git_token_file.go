package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/git"
)

// GIT_ACCESS_TOKEN_FILE reload (Portico fork, poli.6). The Portico master rewrites a one-hour
// GitHub installation token every 30-45 minutes with an atomic rename; upstream reads the file once.
// Here the file is re-read whenever it changes (config.ReloadingFile) and the git auth resolver
// takes the global token from it on every resolution (git.SetGlobalTokenSource).

// gitTokenFilePath returns the token file to reload, or "" to keep the upstream behaviour: no
// file, a GIT_ACCESS_TOKEN in the environment (upstream uses it when the file is empty), or
// complete global GitHub App credentials (validateGitAuthConfig keeps GitHubAppID only then, and
// upstream refuses to combine them with a token).
func gitTokenFilePath(c *app.Config) string {
	if os.Getenv("GIT_ACCESS_TOKEN") != "" || c.GitHubAppID != "" {
		return ""
	}

	return strings.TrimSpace(os.Getenv("GIT_ACCESS_TOKEN_FILE"))
}

// enableGitTokenFileReload installs the live token source. It must run before anything copies c.
func enableGitTokenFileReload(c *app.Config, log *slog.Logger) error {
	path := gitTokenFilePath(c)
	if path == "" {
		return nil
	}

	f, err := config.NewReloadingFile("GIT_ACCESS_TOKEN", path, log)
	if err != nil {
		return fmt.Errorf("GIT_ACCESS_TOKEN_FILE reload: %w", err)
	}

	git.SetGlobalTokenSource(f.Value)

	// Every caller passes c.GitAccessToken to the resolver as an explicit token, which wins over the
	// global one: cleared, they all fall back to the live token.
	c.GitAccessToken = ""

	return nil
}
