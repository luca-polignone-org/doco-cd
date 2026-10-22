package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"github.com/kimdre/doco-cd/internal/commitstatus"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/git"
)

const forkTokenRepoURL = "https://github.com/acme/app.git" // #nosec G101 -- a repository URL, not a credential

func forkWriteToken(t *testing.T, path, content string) {
	t.Helper()

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil { // #nosec G703 -- path is constructed from t.TempDir(), not user input
		t.Fatal(err)
	}

	if err := os.Rename(tmp, path); err != nil { // #nosec G703 -- path is constructed from t.TempDir(), not user input
		t.Fatal(err)
	}
}

// forkTokenSetup starts like main: the token file read once into c, the resolver configured with it.
func forkTokenSetup(t *testing.T, content string) (string, *app.Config) {
	t.Helper()
	t.Cleanup(func() {
		git.SetGlobalTokenSource(nil)
		git.ConfigureAuthResolver(nil, "", "", "", "", git.GitHubAppConfig{})
	})

	path := filepath.Join(t.TempDir(), "git_access_token")
	forkWriteToken(t, path, content)
	t.Setenv("GIT_ACCESS_TOKEN", "")
	t.Setenv("GIT_ACCESS_TOKEN_FILE", path)

	c := &app.Config{GitAccessToken: content, GitScmProvider: "github"}
	git.ConfigureAuthResolver(nil, "", "", c.GitAccessToken, "", git.GitHubAppConfig{})

	return path, c
}

func forkCloneToken(t *testing.T, c *app.Config) string {
	t.Helper()

	auth, err := git.GetAuthMethod(forkTokenRepoURL, c.SSHPrivateKey, c.SSHPrivateKeyPassphrase, c.GitAccessToken)
	if err != nil {
		t.Fatalf("GetAuthMethod() error = %v", err)
	}

	ba, ok := auth.(*githttp.BasicAuth)
	if !ok {
		t.Fatalf("GetAuthMethod() = %T, want *githttp.BasicAuth", auth)
	}

	return ba.Password
}

func TestForkGitTokenFile_LiveTokenForCloneAndCommitStatus(t *testing.T) {
	path, c := forkTokenSetup(t, "tok-one")

	if err := enableGitTokenFileReload(c, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("enableGitTokenFileReload() error = %v", err)
	}

	if c.GitAccessToken != "" {
		t.Fatal("c.GitAccessToken still set: callers would pass the startup token explicitly")
	}

	forkWriteToken(t, path, "tok-two")

	if got := forkCloneToken(t, c); got != "tok-two" {
		t.Fatalf("clone token = %q, want tok-two", got)
	}

	req, ok := commitstatus.ResolveRequest(slog.New(slog.DiscardHandler), commitstatus.RequestParams{
		Enabled:          true,
		SourceIsGit:      true,
		SourceURL:        forkTokenRepoURL,
		CommitSHA:        "0123456789abcdef0123456789abcdef01234567",
		ProviderOverride: c.GitScmProvider,
		AccessToken:      c.GitAccessToken,
	})
	if !ok || req.Token != "tok-two" {
		t.Fatalf("commit status token = %q (ok=%v), want tok-two", req.Token, ok)
	}
}

func TestForkGitTokenFile_EnvTokenWins(t *testing.T) {
	_, c := forkTokenSetup(t, "")
	t.Setenv("GIT_ACCESS_TOKEN", "from-env")

	c.GitAccessToken = "from-env"
	git.ConfigureAuthResolver(nil, "", "", c.GitAccessToken, "", git.GitHubAppConfig{})

	if err := enableGitTokenFileReload(c, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("enableGitTokenFileReload() error = %v", err)
	}

	if c.GitAccessToken != "from-env" || forkCloneToken(t, c) != "from-env" {
		t.Fatal("GIT_ACCESS_TOKEN must stay in use, without reload")
	}
}

func TestForkGitTokenFilePath(t *testing.T) {
	t.Setenv("GIT_ACCESS_TOKEN", "")
	t.Setenv("GIT_ACCESS_TOKEN_FILE", " /run/secrets/git_access_token \n")

	if got := gitTokenFilePath(&app.Config{}); got != "/run/secrets/git_access_token" {
		t.Fatalf("gitTokenFilePath() = %q, want the trimmed path", got)
	}

	if got := gitTokenFilePath(&app.Config{GitHubAppID: "12345", GitHubAppPrivateKey: "key"}); got != "" {
		t.Fatalf("with a global GitHub App: gitTokenFilePath() = %q, want empty", got)
	}

	t.Setenv("GIT_ACCESS_TOKEN_FILE", "")

	if got := gitTokenFilePath(&app.Config{}); got != "" {
		t.Fatalf("without the variable: gitTokenFilePath() = %q, want empty", got)
	}
}

func TestForkGitTokenFile_NoFileIsUpstream(t *testing.T) {
	_, c := forkTokenSetup(t, "startup")
	t.Setenv("GIT_ACCESS_TOKEN_FILE", "")

	if err := enableGitTokenFileReload(c, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("enableGitTokenFileReload() error = %v", err)
	}

	if c.GitAccessToken != "startup" {
		t.Fatalf("c.GitAccessToken = %q, want it untouched", c.GitAccessToken)
	}
}

func TestForkGitTokenFile_FileGoneBeforeWiringFails(t *testing.T) {
	path, c := forkTokenSetup(t, "tok-one")

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	if err := enableGitTokenFileReload(c, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("enableGitTokenFileReload() error = nil, want an error")
	}

	if c.GitAccessToken != "tok-one" {
		t.Fatal("a failed wiring must leave c untouched")
	}
}
