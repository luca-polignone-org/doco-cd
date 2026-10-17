package docker

import (
	"fmt"

	"github.com/docker/cli/cli/command"
)

// ForkJobGate is the fork compose policy gate of jobs that rebuild containers from a deployed
// revision (loadComposeScheduledProjectAll). nil allows everything, so upstream behaviour is
// unchanged; internal/stages sets it when a compose policy is registered, at startup.
var ForkJobGate func(contextName, project string) error

// checkForkJobGate applies ForkJobGate to the project on the Docker context of dockerCli.
func checkForkJobGate(dockerCli command.Cli, project string) error {
	gate := ForkJobGate
	if gate == nil {
		return nil
	}

	contextName := ""
	if dockerCli != nil {
		contextName = dockerCli.CurrentContext()
	}

	if err := gate(contextName, project); err != nil {
		return fmt.Errorf("compose project %s: %w", project, err)
	}

	return nil
}
