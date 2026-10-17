package controlplane

import (
	"fmt"
	"strings"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/git"
)

// RestrictPollConfigs admits only entries of the operator's poll config (POLL_CONFIG or
// POLL_CONFIG_FILE) and returns **the operator's entries**, not the ones of the body: the
// caller chooses which target to re-run, not how (run_once, interval, watch, deployments).
// REST and MCP both use it.
func RestrictPollConfigs(known, requested []poll.Config) ([]poll.Config, error) {
	out := make([]poll.Config, 0, len(requested))

	for i, c := range requested {
		if len(c.Deployments) > 0 {
			return nil, fmt.Errorf("index %d: inline deployments are not allowed", i)
		}

		match, ok := findPoll(known, c)
		if !ok {
			return nil, fmt.Errorf("index %d: not a configured poll target", i)
		}

		out = append(out, match)
	}

	return out, nil
}

func findPoll(known []poll.Config, c poll.Config) (poll.Config, bool) {
	sourceType := config.NormalizeSourceType(c.Source)

	for _, k := range known {
		if config.NormalizeSourceType(k.Source) == sourceType &&
			samePollURL(sourceType, k.SourceUrl, c.SourceUrl) &&
			(k.Reference == c.Reference || git.ReferenceMatches(c.Reference, k.Reference) || git.ReferenceMatches(k.Reference, c.Reference)) &&
			k.CustomTarget == c.CustomTarget {
			return k, true
		}
	}

	return poll.Config{}, false
}

// samePollURL compares source URLs: Git URLs through NormalizeGitURL (local paths become
// file:// URLs), OCI references only with surrounding whitespace trimmed.
func samePollURL(sourceType config.SourceType, a, b string) bool {
	if sourceType == config.SourceTypeOCI {
		return strings.TrimSpace(a) == strings.TrimSpace(b)
	}

	return config.NormalizeGitURL(a) == config.NormalizeGitURL(b)
}
