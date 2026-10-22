package git

import "sync/atomic"

// Live global token (Portico fork, poli.6). ConfigureAuthResolver copies the global token once, at
// startup. With GIT_ACCESS_TOKEN_FILE the fork installs a source that re-reads the file
// (cmd/doco-cd/fork_git_token_file.go), and resolveAuthConfig asks it for the global token on
// every resolution: clone, fetch, poll, remote includes and SCM API calls all go through it.

var globalTokenSource atomic.Pointer[func() string]

// SetGlobalTokenSource makes src the global access token of every later resolution, in place of
// the one given to ConfigureAuthResolver; nil restores that one.
func SetGlobalTokenSource(src func() string) {
	if src == nil {
		globalTokenSource.Store(nil)

		return
	}

	globalTokenSource.Store(&src)
}

// liveGlobalToken returns the token of the installed source, or static without one.
func liveGlobalToken(static string) string {
	if src := globalTokenSource.Load(); src != nil {
		return (*src)()
	}

	return static
}
