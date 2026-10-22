package git

import (
	"testing"

	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

const forkRepoURL = "https://github.com/acme/app.git"

func forkResetTokenSource(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		SetGlobalTokenSource(nil)
		ConfigureAuthResolver(nil, "", "", "", "", GitHubAppConfig{})
	})
}

func forkPassword(t *testing.T, url, token string) string {
	t.Helper()

	auth, err := GetAuthMethod(url, "", "", token)
	if err != nil {
		t.Fatalf("GetAuthMethod() error = %v", err)
	}

	ba, ok := auth.(*githttp.BasicAuth)
	if !ok {
		t.Fatalf("GetAuthMethod() = %T, want *githttp.BasicAuth", auth)
	}

	return ba.Password
}

func TestForkGlobalTokenSource_ReplacesStartupToken(t *testing.T) {
	forkResetTokenSource(t)
	ConfigureAuthResolver(nil, "", "", "startup", "", GitHubAppConfig{})

	live := "first"

	SetGlobalTokenSource(func() string { return live })

	if got := forkPassword(t, forkRepoURL, ""); got != "first" {
		t.Fatalf("password = %q, want first", got)
	}

	live = "second"

	if got := forkPassword(t, forkRepoURL, ""); got != "second" {
		t.Fatalf("password = %q, want second", got)
	}
}

func TestForkGlobalTokenSource_NilRestoresStartupToken(t *testing.T) {
	forkResetTokenSource(t)
	ConfigureAuthResolver(nil, "", "", "startup", "", GitHubAppConfig{})

	SetGlobalTokenSource(func() string { return "live" })
	SetGlobalTokenSource(nil)

	if got := forkPassword(t, forkRepoURL, ""); got != "startup" {
		t.Fatalf("password = %q, want startup", got)
	}
}

func TestForkGlobalTokenSource_ExplicitTokenStillWins(t *testing.T) {
	forkResetTokenSource(t)
	SetGlobalTokenSource(func() string { return "live" })

	if got := forkPassword(t, forkRepoURL, "explicit"); got != "explicit" {
		t.Fatalf("password = %q, want explicit", got)
	}
}

func TestForkGlobalTokenSource_ScopedEntries(t *testing.T) {
	forkResetTokenSource(t)
	ConfigureAuthResolver([]ScopedAuthConfig{
		{Domains: []string{"github.com"}},
		{Domains: []string{"git.example.com"}, GitAccessToken: "scoped-own"},
	}, "", "", "startup", "", GitHubAppConfig{})
	SetGlobalTokenSource(func() string { return "live" })

	if got := forkPassword(t, forkRepoURL, ""); got != "live" {
		t.Fatalf("scoped entry without a token: password = %q, want live", got)
	}

	if got := forkPassword(t, "https://git.example.com/acme/app.git", ""); got != "scoped-own" {
		t.Fatalf("scoped entry with its token: password = %q, want scoped-own", got)
	}
}

func TestForkGlobalTokenSource_ResolveHTTPToken(t *testing.T) {
	forkResetTokenSource(t)
	SetGlobalTokenSource(func() string { return "live" })

	token, err := ResolveHTTPToken(forkRepoURL, ResolveAuthConfig(forkRepoURL, "", "", ""))
	if err != nil || token != "live" {
		t.Fatalf("ResolveHTTPToken() = %q, %v, want live, nil", token, err)
	}
}
