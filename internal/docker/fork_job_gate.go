package docker

import (
	"fmt"

	"github.com/docker/cli/cli/command"

	"github.com/kimdre/doco-cd/internal/config/deploy"
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

	if err := gate(forkJobContext(dockerCli), project); err != nil {
		return fmt.Errorf("compose project %s: %w", project, err)
	}

	return nil
}

// forkJobContext is the Docker context of a job: the current context of dockerCli, empty without
// one (callers that reload a project without a Docker client, such as certificate rotation tests).
func forkJobContext(dockerCli command.Cli) string {
	if dockerCli == nil {
		return ""
	}

	return dockerCli.CurrentContext()
}

// forkJobObject is the object the job gate checks for project: the secret gate of the same job
// (withForkJobObject) uses it, so both gates see the same <context>/<project>.
func forkJobObject(dockerCli command.Cli, project string) string {
	return deploy.ObjectName(forkJobContext(dockerCli), project)
}
